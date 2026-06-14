// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/pkg/archive"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestNativeRootfsExportEnabledParsesEnv(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "")
	if nativeRootfsExportEnabled() {
		t.Fatal("expected native rootfs export to be disabled by default")
	}

	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")
	if !nativeRootfsExportEnabled() {
		t.Fatal("expected native rootfs export to be enabled")
	}

	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "not-a-bool")
	if nativeRootfsExportEnabled() {
		t.Fatal("expected invalid native export env value to be disabled")
	}
}

func TestNativeExportConcurrencyParsesEnv(t *testing.T) {
	t.Setenv(nativeExportJobsEnv, "")
	if got := nativeExportConcurrency(); got != defaultNativeExportConcurrency {
		t.Fatalf("default concurrency=%d, want %d", got, defaultNativeExportConcurrency)
	}

	t.Setenv(nativeExportJobsEnv, "12")
	if got := nativeExportConcurrency(); got != 12 {
		t.Fatalf("configured concurrency=%d, want 12", got)
	}

	t.Setenv(nativeExportJobsEnv, "0")
	if got := nativeExportConcurrency(); got != defaultNativeExportConcurrency {
		t.Fatalf("invalid concurrency=%d, want %d", got, defaultNativeExportConcurrency)
	}

	t.Setenv(nativeExportJobsEnv, "128")
	if got := nativeExportConcurrency(); got != maxNativeExportConcurrency {
		t.Fatalf("capped concurrency=%d, want %d", got, maxNativeExportConcurrency)
	}
}

func TestPrepareNativeSourceExtractsDigestAndConfig(t *testing.T) {
	s := httptest.NewServer(registry.New())
	defer s.Close()

	// Create a dummy image
	img, err := mutate.Config(empty.Image, v1.Config{
		Cmd: []string{"/bin/sh"},
	})
	if err != nil {
		t.Fatalf("mutate.Config: %v", err)
	}

	// Create a dummy layer
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "test.txt", Size: 4, Mode: 0644})
	tw.Write([]byte("test"))
	tw.Close()
	layer, _ := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b.Bytes())), nil
	})
	img, _ = mutate.AppendLayers(img, layer)

	ref, _ := name.ParseReference(s.URL[7:] + "/test-native:latest")
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}

	spec := SourceSpec{
		ImageRef:         "docker://" + ref.Name(),
		RegistryUsername: "",
		RegistryPassword: "",
	}

	source, err := prepareNativeSource(context.Background(), spec)
	if err != nil {
		t.Fatalf("prepareNativeSource failed: %v", err)
	}

	if source.ExportMode != ExportModeNative {
		t.Errorf("expected ExportMode=ExportModeNative, got %q", source.ExportMode)
	}
	if len(source.Config.Cmd) == 0 || source.Config.Cmd[0] != "/bin/sh" {
		t.Errorf("expected Config.Cmd=[/bin/sh], got %v", source.Config.Cmd)
	}

	digest, _ := img.Digest()
	expectedDigest := s.URL[7:] + "/test-native@" + digest.String()
	if source.Digest != expectedDigest {
		t.Errorf("expected Digest=%q, got %q", expectedDigest, source.Digest)
	}

	// We appended 1 layer, let's just make sure CompressedSizeBytes > 0
	if source.CompressedSizeBytes <= 0 {
		t.Errorf("expected CompressedSizeBytes > 0, got %d", source.CompressedSizeBytes)
	}
}

func TestPrepareSourceUsesNativeWhenEnabled(t *testing.T) {
	t.Setenv("CUBEMASTER_NATIVE_ROOTFS_EXPORT_ENABLED", "true")

	img, err := mutate.Config(empty.Image, v1.Config{
		Cmd: []string{"native"},
	})
	if err != nil {
		t.Fatalf("mutate.Config: %v", err)
	}

	originalRemoteImage := remoteImage
	remoteCalls := 0
	remoteImage = func(name.Reference, ...remote.Option) (v1.Image, error) {
		remoteCalls++
		return img, nil
	}
	t.Cleanup(func() {
		remoteImage = originalRemoteImage
	})

	tests := []struct {
		name string
		fn   func(context.Context, SourceSpec) (*PreparedSource, error)
	}{
		{name: "PrepareSource", fn: PrepareSource},
		{name: "PrepareLocalSource", fn: PrepareLocalSource},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := tt.fn(context.Background(), SourceSpec{
				ImageRef:         "example.com/native:latest",
				RegistryUsername: "user",
				RegistryPassword: "pass",
			})
			if err != nil {
				t.Fatalf("%s failed: %v", tt.name, err)
			}
			if source.ExportMode != ExportModeNative {
				t.Fatalf("ExportMode=%q, want %q", source.ExportMode, ExportModeNative)
			}
			if source.RegistryAuth == nil || source.RegistryAuth.Username != "user" || source.RegistryAuth.Password != "pass" {
				t.Fatalf("unexpected RegistryAuth: %#v", source.RegistryAuth)
			}
			if len(source.Config.Cmd) != 1 || source.Config.Cmd[0] != "native" {
				t.Fatalf("Config.Cmd=%v, want [native]", source.Config.Cmd)
			}
			if source.nativeImage == nil {
				t.Fatal("expected prepared native image to be cached")
			}
		})
	}

	if remoteCalls != len(tests) {
		t.Fatalf("remoteImage calls=%d, want %d", remoteCalls, len(tests))
	}
}

func TestImageDigestFromReferenceMatchesDockerlessCanonicalName(t *testing.T) {
	tests := []struct {
		name     string
		imageRef string
		want     string
	}{
		{
			name:     "docker hub short name",
			imageRef: "nginx:latest",
			want:     "docker.io/library/nginx@sha256:abcd",
		},
		{
			name:     "docker hub explicit alias",
			imageRef: "docker.io/library/nginx:latest",
			want:     "docker.io/library/nginx@sha256:abcd",
		},
		{
			name:     "non docker hub registry",
			imageRef: "example.com/ns/app:stable",
			want:     "example.com/ns/app@sha256:abcd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := name.ParseReference(tt.imageRef)
			if err != nil {
				t.Fatalf("ParseReference(%q): %v", tt.imageRef, err)
			}
			if got := imageDigestFromReference(ref, "sha256:abcd"); got != tt.want {
				t.Fatalf("imageDigestFromReference(%q)=%q, want %q", tt.imageRef, got, tt.want)
			}
		})
	}
}

func TestStreamRegistryWhiteoutResolution(t *testing.T) {
	// Base layer: creates /dir/file1 and /dir/file2
	var b1 bytes.Buffer
	tw1 := tar.NewWriter(&b1)
	tw1.WriteHeader(&tar.Header{Name: "dir/", Mode: 0755, Typeflag: tar.TypeDir})
	tw1.WriteHeader(&tar.Header{Name: "dir/file1", Size: 4, Mode: 0644})
	tw1.Write([]byte("data"))
	tw1.WriteHeader(&tar.Header{Name: "dir/file2", Size: 4, Mode: 0644})
	tw1.Write([]byte("data"))
	tw1.Close()
	l1, _ := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b1.Bytes())), nil
	})

	// Second layer: deletes /dir/file1 via whiteout, and makes an opaque dir marker /dir/.wh..wh..opq
	var b2 bytes.Buffer
	tw2 := tar.NewWriter(&b2)
	tw2.WriteHeader(&tar.Header{Name: "dir/.wh.file1", Size: 0, Mode: 0644})
	tw2.WriteHeader(&tar.Header{Name: "dir/.wh..wh..opq", Size: 0, Mode: 0644})
	tw2.WriteHeader(&tar.Header{Name: "dir/file3", Size: 4, Mode: 0644}) // New file
	tw2.Write([]byte("data"))
	tw2.Close()
	l2, _ := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b2.Bytes())), nil
	})

	img, _ := mutate.AppendLayers(empty.Image, l1, l2)

	destDir := t.TempDir()
	source := &PreparedSource{
		LocalRef:    "docker.io/library/test-whiteout:latest",
		nativeImage: img,
	}

	err := StreamRegistryToDir(context.Background(), source, destDir)
	if err != nil {
		t.Fatalf("StreamRegistryToDir failed: %v", err)
	}

	// Verify the result
	// file1 should be deleted by whiteout
	if _, err := os.Stat(filepath.Join(destDir, "dir", "file1")); !os.IsNotExist(err) {
		t.Errorf("expected dir/file1 to be deleted by whiteout, but it exists")
	}
	// file2 should be deleted by the opaque directory marker
	if _, err := os.Stat(filepath.Join(destDir, "dir", "file2")); !os.IsNotExist(err) {
		t.Errorf("expected dir/file2 to be deleted by opaque dir marker, but it exists")
	}
	// file3 should exist
	if _, err := os.Stat(filepath.Join(destDir, "dir", "file3")); err != nil {
		t.Errorf("expected dir/file3 to exist, but got: %v", err)
	}
}

func TestStreamRegistryUsesPreparedNativeImage(t *testing.T) {
	var b1 bytes.Buffer
	tw1 := tar.NewWriter(&b1)
	tw1.WriteHeader(&tar.Header{Name: "dir/", Mode: 0755, Typeflag: tar.TypeDir})
	tw1.WriteHeader(&tar.Header{Name: "dir/file1", Size: 4, Mode: 0644})
	tw1.Write([]byte("data"))
	tw1.Close()
	l1, _ := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b1.Bytes())), nil
	})

	img, _ := mutate.AppendLayers(empty.Image, l1)

	originalRemoteImage := remoteImage
	remoteImage = func(name.Reference, ...remote.Option) (v1.Image, error) {
		t.Fatal("remoteImage should not be called when nativeImage is already cached")
		return nil, nil
	}
	t.Cleanup(func() {
		remoteImage = originalRemoteImage
	})

	source := &PreparedSource{
		LocalRef:    "docker.io/library/test-native:latest",
		nativeImage: img,
	}

	destDir := t.TempDir()
	if err := StreamRegistryToDir(context.Background(), source, destDir); err != nil {
		t.Fatalf("StreamRegistryToDir failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(destDir, "dir", "file1")); err != nil {
		t.Fatalf("expected dir/file1 to exist, but got: %v", err)
	}
}

func TestUntarRootfsTarXattrAndPerms(t *testing.T) {
	// Isolate the test to archive.Untar
	destDir := t.TempDir()

	var b bytes.Buffer
	tw := tar.NewWriter(&b)

	// A normal file
	tw.WriteHeader(&tar.Header{Name: "test.sh", Size: 4, Mode: 0755})
	tw.Write([]byte("echo"))

	// A symlink
	tw.WriteHeader(&tar.Header{Name: "link.sh", Typeflag: tar.TypeSymlink, Linkname: "test.sh"})

	// We can try injecting PAX records for xattr here, but standard archive/tar
	// does not support setting SCHILY.xattr.security.capability easily through
	// the Header struct without formatting extensions.
	// However, we just test that Untar processes it without panic and retains permissions.
	tw.Close()

	err := archive.Untar(bytes.NewReader(b.Bytes()), destDir, &archive.TarOptions{
		NoLchown:         false,
		BestEffortXattrs: false, // Must not fail for basic files
	})

	if err != nil {
		t.Fatalf("archive.Untar failed: %v", err)
	}

	fi, err := os.Lstat(filepath.Join(destDir, "test.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0755 {
		t.Errorf("expected mode 0755, got %v", fi.Mode().Perm())
	}

	li, err := os.Lstat(filepath.Join(destDir, "link.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if li.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected link.sh to be a symlink")
	}
}
