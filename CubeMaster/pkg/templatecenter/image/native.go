// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/docker/docker/pkg/archive"
	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/sync/errgroup"
)

const (
	defaultNativeExportConcurrency = 6
	maxNativeExportConcurrency     = 32
	nativeExportJobsEnv            = "CUBEMASTER_NATIVE_ROOTFS_EXPORT_JOBS"
)

// The following variables allow tests to stub out remote registry calls
// without relying heavily on gomonkey patches.
var (
	remoteImage = remote.Image
)

// nativeRootfsExportEnabled returns true if the CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED
// environment variable is set to true. When enabled, this avoids the use of external
// CLI tools like docker, skopeo, and umoci.
func nativeRootfsExportEnabled() bool {
	v, _ := strconv.ParseBool(os.Getenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED"))
	return v
}

// registryAuthOption converts the PreparedSource credentials into a remote.Option.
// Passed via pointer to ensure it is nil-safe. If credentials are explicitly
// provided, they are used. Otherwise, it falls back to the DefaultKeychain
// which reads from ~/.docker/config.json, DOCKER_CONFIG, or XDG_RUNTIME_DIR.
func registryAuthOption(auth *RegistryAuthConfig) remote.Option {
	if auth != nil && (auth.Username != "" || auth.Password != "") {
		return remote.WithAuth(authn.FromConfig(authn.AuthConfig{
			Username: auth.Username,
			Password: auth.Password,
		}))
	}
	return remote.WithAuthFromKeychain(authn.DefaultKeychain)
}

// StreamRegistryToDir fetches the image directly from the registry using
// go-containerregistry and extracts its flattened filesystem (via mutate.Extract)
// directly into destDir using docker's archive.Untar. This avoids intermediate
// local caching and avoids spawning docker/skopeo processes.
func StreamRegistryToDir(ctx context.Context, source *PreparedSource, destDir string) error {
	img, err := nativeImageForSource(ctx, source)
	if err != nil {
		return err
	}

	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("native export failed to get layers: %w", err)
	}

	// Phase 1: Concurrently prefetch compressed layers to disk to saturate network bandwidth
	// and eliminate the risk of networking I/O blocking disk I/O.
	// We create the temporary directory in the same workspace (filepath.Dir(destDir))
	// to ensure it utilizes the fast build disk.
	prefetchDir, err := os.MkdirTemp(filepath.Dir(destDir), "native-prefetch-*")
	if err != nil {
		return fmt.Errorf("native export failed to create prefetch dir: %w", err)
	}
	defer os.RemoveAll(prefetchDir)

	paths := make([]string, len(layers))
	jobs := nativeExportConcurrency()
	sem := make(chan struct{}, jobs)

	eg, egCtx := errgroup.WithContext(ctx)

	for i, l := range layers {
		layerIdx := i
		layer := l
		eg.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-egCtx.Done():
				return egCtx.Err()
			}
			defer func() { <-sem }()

			rc, err := layer.Compressed()
			if err != nil {
				return fmt.Errorf("failed to open compressed stream for layer %d: %w", layerIdx, err)
			}
			defer rc.Close()

			f, err := os.CreateTemp(prefetchDir, fmt.Sprintf("layer-%03d-*.tar", layerIdx))
			if err != nil {
				return fmt.Errorf("failed to create temp file for layer %d: %w", layerIdx, err)
			}
			path := f.Name()
			if err := f.Chmod(0600); err != nil {
				_ = f.Close()
				return fmt.Errorf("failed to restrict temp file permissions for layer %d: %w", layerIdx, err)
			}

			if _, err := io.Copy(f, contextReader{ctx: egCtx, r: rc}); err != nil {
				_ = f.Close()
				return fmt.Errorf("failed to download layer %d: %w", layerIdx, err)
			}

			if err := f.Close(); err != nil {
				return fmt.Errorf("failed to close temp file for layer %d: %w", layerIdx, err)
			}

			paths[layerIdx] = path
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	// Phase 2: Sequentially decompress and apply to the final destination
	opts := &archive.TarOptions{
		NoLchown:         false,
		BestEffortXattrs: true,
	}

	for i, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("native export failed to open prefetched layer %d: %w", i, err)
		}

		decompressed, err := archive.DecompressStream(f)
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("native export failed to decompress layer %d: %w", i, err)
		}

		_, applyErr := archive.ApplyUncompressedLayer(destDir, decompressed, opts)
		_ = decompressed.Close()
		_ = f.Close() // safe to double close, ensures FD is freed immediately

		if applyErr != nil {
			return fmt.Errorf("native export failed to apply layer %d to %q (Hint: this might require root privileges, CAP_MKNOD, or a destination filesystem that supports xattrs/capabilities): %w", i, destDir, applyErr)
		}

		// Immediately delete the compressed temp file after successful extraction
		// to minimize peak disk usage.
		_ = os.Remove(path)
	}

	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

func nativeImageForSource(ctx context.Context, source *PreparedSource) (v1.Image, error) {
	if source == nil {
		return nil, fmt.Errorf("native export source is nil")
	}
	if source.nativeImage != nil {
		return source.nativeImage, nil
	}
	return nil, fmt.Errorf("native export source image was not prepared")
}

func nativeExportConcurrency() int {
	raw := os.Getenv(nativeExportJobsEnv)
	if raw == "" {
		return defaultNativeExportConcurrency
	}
	jobs, err := strconv.Atoi(raw)
	if err != nil || jobs <= 0 {
		return defaultNativeExportConcurrency
	}
	if jobs > maxNativeExportConcurrency {
		return maxNativeExportConcurrency
	}
	return jobs
}

func defaultPlatform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}
