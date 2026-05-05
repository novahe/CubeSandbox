package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var globalID int32

const iterations = 4

// ==========================================
// Cgroup Utils
// ==========================================

func getCgroupMode() int {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	if err == nil {
		return 2
	}
	return 1
}

func createCgroupV1(groupName string, pid int) error {
	subsystems := []string{"cpu", "memory"}
	for _, sub := range subsystems {
		path := filepath.Join("/sys/fs/cgroup", sub, groupName)
		if err := os.MkdirAll(path, 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "tasks"), []byte(fmt.Sprintf("%d", pid)), 0644); err != nil {
			return err
		}
	}
	return nil
}

func ptrInt64(i int64) *int64   { return &i }
func ptrUint64(i uint64) *uint64 { return &i }

// ==========================================
// Benchmark Functions (NO cleanup during test)
// ==========================================

func createCgroupAndProcess(id int) error {
	groupName := fmt.Sprintf("bench-cg-%d", id)

	cmd := exec.Command("sleep", "3600")
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid

	mode := getCgroupMode()
	if mode == 2 {
		res := &cgroup2.Resources{
			CPU:    &cgroup2.CPU{Max: cgroup2.NewCPUMax(ptrInt64(100000), ptrUint64(100000))},
			Memory: &cgroup2.Memory{Max: ptrInt64(512 * 1024 * 1024)},
		}
		mgr, err := cgroup2.NewManager("/sys/fs/cgroup", "/"+groupName, res)
		if err != nil {
			return err
		}
		if err := mgr.AddProc(uint64(pid)); err != nil {
			return err
		}
	} else {
		if err := createCgroupV1(groupName, pid); err != nil {
			return err
		}
	}

	return nil
}

func createTap(id int) error {
	name := fmt.Sprintf("bench-tap-%d", id)
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		Mode:      netlink.TUNTAP_MODE_TAP,
		Flags:     netlink.TUNTAP_DEFAULTS | unix.IFF_VNET_HDR,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		return err
	}
	return nil
}

// ==========================================
// Test Runner
// ==========================================

func runBurstBenchmark(name string, count int, taskFn func(id int) error) {
	var wg sync.WaitGroup
	var errorCount int32

	fmt.Printf(">>> [%s] Burst Size=%d ...\n", name, count)

	start := time.Now()
	for i := 0; i < count; i++ {
		wg.Add(1)
		id := atomic.AddInt32(&globalID, 1)
		go func(myID int) {
			defer wg.Done()
			if err := taskFn(myID); err != nil {
				atomic.AddInt32(&errorCount, 1)
			}
		}(int(id))
	}
	wg.Wait()
	totalElapsed := time.Since(start)

	errRate := float64(errorCount) / float64(count) * 100
	fmt.Printf("    Success: %d | Errors: %d (%.2f%%)\n", count-int(errorCount), errorCount, errRate)
	fmt.Printf("    Total Batch Latency: %v\n\n", totalElapsed)
}

// ==========================================
// Cleanup with backoff retry
// ==========================================

func killBenchProcesses() {
	_ = exec.Command("pkill", "-f", "sleep 3600").Run()
	// Wait for processes to actually terminate
	time.Sleep(2 * time.Second)
}

func cleanupTaps() {
	maxRetries := 10
	for r := 0; r < maxRetries; r++ {
		links, err := netlink.LinkList()
		if err != nil {
			time.Sleep(time.Second)
			continue
		}

		var pending []netlink.Link
		for _, link := range links {
			if name := link.Attrs().Name; strings.HasPrefix(name, "bench-tap-") {
				pending = append(pending, link)
			}
		}
		if len(pending) == 0 {
			return
		}

		for _, link := range pending {
			_ = netlink.LinkDel(link)
		}
		time.Sleep(time.Duration(r+1) * 500 * time.Millisecond)
	}
}

func cleanupCgroups() {
	if getCgroupMode() == 2 {
		entries, _ := os.ReadDir("/sys/fs/cgroup")
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "bench-cg-") {
				mgr, err := cgroup2.Load("/" + entry.Name())
				if err == nil && mgr != nil {
					_ = mgr.Delete()
				}
			}
		}
	} else {
		for _, sub := range []string{"cpu", "memory"} {
			entries, _ := os.ReadDir(filepath.Join("/sys/fs/cgroup", sub))
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), "bench-cg-") {
					_ = os.Remove(filepath.Join("/sys/fs/cgroup", sub, entry.Name()))
				}
			}
		}
	}
}

func cleanupEverything() {
	fmt.Println(">>> Final Cleanup: killing processes...")
	killBenchProcesses()

	fmt.Println(">>> Final Cleanup: removing cgroups...")
	cleanupCgroups()

	fmt.Println(">>> Final Cleanup: removing tap devices...")
	cleanupTaps()

	fmt.Println(">>> Cleanup finished.")
}

// ==========================================
// Main
// ==========================================

func main() {
	burstSizes := []int{10, 100, 200}

	fmt.Println("==================================================")
	fmt.Println(" Cgroup Burst Test (no deletion during test)")
	fmt.Printf(" %d iterations per burst size, 30s cooldown\n", iterations)
	fmt.Println("==================================================")
	for _, size := range burstSizes {
		for i := 1; i <= iterations; i++ {
			fmt.Printf("--- Iteration %d/%d ---\n", i, iterations)
			runBurstBenchmark("Cgroup", size, createCgroupAndProcess)
		}
		// Cleanup after all iterations of this burst size
		cleanupEverything()
		fmt.Printf(">>> Sleeping 30s for resource recovery...\n\n")
		time.Sleep(30 * time.Second)
	}

	fmt.Println("==================================================")
	fmt.Println(" Tap Burst Test (no deletion during test)")
	fmt.Printf(" %d iterations per burst size, 30s cooldown\n", iterations)
	fmt.Println("==================================================")
	for _, size := range burstSizes {
		for i := 1; i <= iterations; i++ {
			fmt.Printf("--- Iteration %d/%d ---\n", i, iterations)
			runBurstBenchmark("Tap", size, createTap)
		}
		// Cleanup after all iterations of this burst size
		cleanupEverything()
		fmt.Printf(">>> Sleeping 30s for resource recovery...\n\n")
		time.Sleep(30 * time.Second)
	}
}
