package main

import (
	"os"
	"sync"
	"testing"
	"time"
)

// TestStatsCacheConcurrent exercises the warm cache from many goroutines at
// once so `go test -race` proves the locking is sound. The background collector
// writes while every page request reads concurrently in production.
func TestStatsCacheConcurrent(t *testing.T) {
	c := newStatsCache()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			stats := map[string]containerStat{
				"abc": {cpuPerc: "1.0%", memBytes: int64(n), cpuFloat: float64(n)},
			}
			c.store(stats, n, HostUsage{CPUPercent: float64(n)}, 5*time.Millisecond, "", time.Now())
			c.storeHost(hostInfo{cpuCores: 4, memTotal: 1 << 30}, time.Now())
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, _ = c.read()
			_ = c.usageSnapshot()
			_, _ = c.hostSnapshot()
			_, _, _, _ = c.snapshot()
		}()
	}
	wg.Wait()

	if _, _, images, _ := c.read(); images < 0 {
		t.Fatalf("unexpected negative image count: %d", images)
	}
	if _, _, ready, _ := c.snapshot(); !ready {
		t.Fatal("cache should report ready after stores")
	}
}

// TestStatsCacheNotReady verifies a freshly-built cache reports "not ready" so
// /healthz can return 503 until the first collection lands.
func TestStatsCacheNotReady(t *testing.T) {
	c := newStatsCache()
	ageMS, _, ready, _ := c.snapshot()
	if ready {
		t.Fatal("a new cache must not be ready")
	}
	if ageMS != 0 {
		t.Fatalf("age should be 0 before first collection, got %d", ageMS)
	}
	if _, age := c.hostSnapshot(); age < time.Hour {
		t.Fatalf("uncollected host info should read as very stale, got %s", age)
	}
}

// TestComputeHostUsage checks the pure usage math derived from per-container
// stats plus cached host facts (no Docker calls involved).
func TestComputeHostUsage(t *testing.T) {
	stats := map[string]containerStat{
		"a": {cpuFloat: 50, memBytes: 1 << 30}, // 1 GiB
		"b": {cpuFloat: 50, memBytes: 1 << 30}, // 1 GiB
	}
	host := hostInfo{cpuCores: 4, memTotal: 8 << 30, diskUsed: 50 << 30, diskTotal: 100 << 30}

	u := computeHostUsage(stats, host)

	// 100% summed CPU across 4 cores => 25% host CPU.
	if got := round1(u.CPUPercent); got != 25.0 {
		t.Errorf("CPUPercent = %.2f, want 25.0", got)
	}
	// 2 GiB of 8 GiB => 25% memory.
	if got := round1(u.MemPercent); got != 25.0 {
		t.Errorf("MemPercent = %.2f, want 25.0", got)
	}
	if got := round1(u.DiskPercent); got != 50.0 {
		t.Errorf("DiskPercent = %.2f, want 50.0", got)
	}
	if u.CPUCores != 4 {
		t.Errorf("CPUCores = %d, want 4", u.CPUCores)
	}
}

// TestComputeHostUsageZeroHost guards the divide-by-zero paths before host
// facts have been collected.
func TestComputeHostUsageZeroHost(t *testing.T) {
	u := computeHostUsage(map[string]containerStat{"a": {cpuFloat: 10, memBytes: 1 << 20}}, hostInfo{})
	if u.CPUPercent != 0 || u.MemPercent != 0 || u.DiskPercent != 0 {
		t.Errorf("percentages must be 0 when host facts are unknown, got cpu=%.2f mem=%.2f disk=%.2f",
			u.CPUPercent, u.MemPercent, u.DiskPercent)
	}
}

func TestEnvDurationOrDefault(t *testing.T) {
	const key = "DOCKPILOT_TEST_DURATION"
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"", 3 * time.Second},   // unset -> fallback
		{"5s", 5 * time.Second}, // duration string
		{"250ms", 250 * time.Millisecond},
		{"10", 10 * time.Second},     // bare integer -> seconds
		{"garbage", 3 * time.Second}, // invalid -> fallback
		{"-4s", 3 * time.Second},     // non-positive -> fallback
	}
	for _, tc := range cases {
		if tc.val == "" {
			os.Unsetenv(key)
		} else {
			os.Setenv(key, tc.val)
		}
		if got := envDurationOrDefault(key, 3*time.Second); got != tc.want {
			t.Errorf("envDurationOrDefault(%q) = %s, want %s", tc.val, got, tc.want)
		}
	}
	os.Unsetenv(key)
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
