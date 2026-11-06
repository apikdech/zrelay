//go:build chaos

package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/app"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage/storagetest"
)

const (
	chaosMaxMemoryMB = 256
	chaosWarmup      = 45 * time.Second
	chaosRecovery    = 3 * time.Minute
)

// chaosOutage is how long MinIO stays black-holed: ten real minutes, or the
// duration in ZRELAY_CHAOS_OUTAGE for a shorter diagnostic run.
func chaosOutage(t *testing.T) time.Duration {
	if v := os.Getenv("ZRELAY_CHAOS_OUTAGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("ZRELAY_CHAOS_OUTAGE: %v", err)
		}
		return d
	}
	return 10 * time.Minute
}

// memClasses are the runtime memory classes sampled besides the total.
var memClasses = []string{
	"/memory/classes/total:bytes",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/free:bytes",
	"/memory/classes/heap/released:bytes",
	"/memory/classes/heap/unused:bytes",
	"/memory/classes/heap/stacks:bytes",
	"/memory/classes/os-stacks:bytes",
	"/memory/classes/metadata/other:bytes",
	"/memory/classes/other:bytes",
}

// TestChaos is the design §9 chaos run: three 4 Mbps cameras whose triggers
// stay active, a 128 MiB backlog, and MinIO (behind a fault proxy) black-holed
// for ten real minutes. It asserts that the breaker opens then closes, that
// dropped clips show up in coverage, that clips are committed again after the
// outage, and that the Go runtime never holds more than max_memory_mb.
//
// The memory criterion is /memory/classes/total:bytes, as the plan specifies,
// not the figure net of memory returned to the OS.
//
// It takes about fifteen minutes and is not part of `make test` or
// `make integration`: run it with `make chaos`.
func TestChaos(t *testing.T) {
	outage := chaosOutage(t)
	m := storagetest.StartRestartableMinIO(t)
	proxy := storagetest.StartFaultProxy(t, m.Endpoint)

	// 4 Mbps is 500 kB/s: about 33 kB per access unit at the fixture's 15 fps.
	cams := startFakeCameras(t, []string{"cam-1", "cam-2", "cam-3"}, 33<<10)
	mk := newMasterKey(t)
	cfgPath := writeS3Config(t, t.TempDir(), s3Opts{
		Endpoint: proxy.URL(), AccessKey: m.AccessKey, SecretKey: m.SecretKey, Bucket: m.Bucket, MasterKey: mk,
		HTTPListen: freePort(t), BacklogMB: 128, MaxMemoryMB: chaosMaxMemoryMB, GraceSeconds: 30,
		MaxClipSeconds: 30, Cameras: cams,
	})
	cfg := loadConfig(t, cfgPath)
	// The run must be a configuration "zrelay config check" accepts: the
	// memory assertion below tests the budget, not an over-committed config.
	budget := config.Budget(cfg)
	budget.WriteTable(os.Stderr)
	if !budget.OK {
		t.Fatalf("chaos config is over the memory budget: %d > %d", budget.Total, budget.Limit)
	}

	logs := captureLogs(t)
	t.Cleanup(func() {
		if t.Failed() { // the relay's own account of what happened
			lines := strings.Split(logs.text(), "\n")
			t.Logf("last relay log lines:\n%s", strings.Join(lines[max(0, len(lines)-60):], "\n"))
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, cfg, app.Options{Version: "chaos"}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Minute):
		}
	})

	// Sample the runtime's total memory every second.
	var peakTotal, peakHeld atomic.Uint64
	var peakDetail atomic.Value // string: the classes when the total peaked
	stopSampling := make(chan struct{})
	var sampling sync.WaitGroup
	sampling.Add(1)
	go func() {
		defer sampling.Done()
		sample := make([]metrics.Sample, len(memClasses))
		for i, n := range memClasses {
			sample[i].Name = n
		}
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-tick.C:
				metrics.Read(sample)
				total, released := sample[0].Value.Uint64(), sample[3].Value.Uint64()
				// What debug.SetMemoryLimit bounds; logged for diagnosis only.
				if v := total - released; v > peakHeld.Load() {
					peakHeld.Store(v)
				}
				if total > peakTotal.Load() {
					peakTotal.Store(total)
					var b strings.Builder
					for _, sm := range sample {
						fmt.Fprintf(&b, " %s=%.1fMiB", strings.TrimSuffix(strings.TrimPrefix(sm.Name, "/memory/classes/"), ":bytes"), float64(sm.Value.Uint64())/(1<<20))
					}
					peakDetail.Store(b.String())
				}
			}
		}
	}()

	// Triggers stay active: renew them well inside trigger_timeout_seconds.
	stopTriggers := make(chan struct{})
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			for _, c := range cams {
				c.motion(true)
			}
			select {
			case <-stopTriggers:
				return
			case <-tick.C:
			}
		}
	}()

	drv := newS3(t, m.Endpoint, m.AccessKey, m.SecretKey, m.Bucket)
	committed := func(since time.Time) int {
		n := 0
		for _, c := range cams {
			objs, err := listObjects(context.Background(), drv, "clips/"+c.id)
			if err != nil {
				continue
			}
			for _, o := range objs {
				if strings.HasSuffix(o.Key, ".json") && o.ModTime.After(since) {
					n++
				}
			}
		}
		return n
	}

	// Warm up: clips flow while storage is healthy.
	eventually(t, "a clip committed before the outage", chaosWarmup+2*time.Minute, func() bool { return committed(time.Time{}) > 0 })
	t.Logf("warm-up done; peak memory so far %.1f MiB", float64(peakTotal.Load())/(1<<20))

	// The outage.
	proxy.Blackhole(true)
	outageStart := time.Now()
	eventually(t, "the breaker to open", 5*time.Minute, func() bool {
		return strings.Contains(logs.text(), "storage breaker state=open")
	})
	t.Logf("breaker opened %v into the outage", time.Since(outageStart).Round(time.Second))
	time.Sleep(time.Until(outageStart.Add(outage)))
	proxy.Blackhole(false)
	healed := time.Now()
	t.Logf("storage restored; peak memory %.1f MiB", float64(peakTotal.Load())/(1<<20))

	eventually(t, "the breaker to close again", chaosRecovery, func() bool {
		s := logs.text()
		i := strings.LastIndex(s, "storage breaker state=open")
		return i >= 0 && strings.Contains(s[i:], "storage breaker state=closed")
	})
	eventually(t, "a clip committed after the outage", chaosRecovery, func() bool { return committed(healed) > 0 })

	close(stopTriggers)
	stopped := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- nil // for the cleanup
	case <-time.After(45 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if took := time.Since(stopped); took > 30*time.Second {
		t.Errorf("shutdown took %v, want <= shutdown_grace_seconds (30s)", took)
	}
	close(stopSampling)
	sampling.Wait()

	// The design §9 and Task 29 criterion: /memory/classes/total:bytes, which
	// keeps counting pages the runtime already returned to the OS, never goes
	// above max_memory_mb.
	t.Logf("/memory/classes/total:bytes peaked at %.1f MiB of %d MiB (total minus released: %.1f MiB)",
		float64(peakTotal.Load())/(1<<20), chaosMaxMemoryMB, float64(peakHeld.Load())/(1<<20))
	if d, ok := peakDetail.Load().(string); ok {
		t.Logf("memory classes at the peak:%s", d)
	}
	if p := peakTotal.Load(); p > chaosMaxMemoryMB<<20 {
		t.Errorf("/memory/classes/total:bytes peaked at %.1f MiB, want <= %d MiB", float64(p)/(1<<20), chaosMaxMemoryMB)
	}

	// Dropped clips are in coverage. The shutdown flush wrote the running
	// hour; earlier hours were written by the publisher.
	drops := map[string]int{}
	var objs int
	objsInfo, err := listObjects(context.Background(), drv, "status/coverage")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objsInfo {
		raw, err := readObject(context.Background(), drv, o.Key)
		if err != nil {
			t.Fatal(err)
		}
		var cov sidecar.Coverage
		if err := json.Unmarshal(raw, &cov); err != nil {
			t.Fatalf("%s: %v", o.Key, err)
		}
		objs++
		for _, d := range cov.Dropped {
			drops[d.Reason]++
		}
	}
	t.Logf("%d coverage objects; drops by reason: %v", objs, drops)
	if drops["backlog"] == 0 {
		t.Errorf("no backlog drop in coverage (drops %v): 3 x 4 Mbps for %v must overflow a 128 MiB backlog", drops, outage)
	}
	if !strings.Contains(logs.text(), fmt.Sprintf("reason=%s", "backlog")) {
		t.Error("no clip drop was logged with reason=backlog")
	}
}
