package config

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

const mib = int64(1) << 20

// Memory budget constants (design §8.1, amending relay-spec §3.2).
const (
	// budgetFPSEstimate is fps_est, the frame rate the queue term assumes.
	budgetFPSEstimate = 15
	// runtimeBytes is the fixed Go runtime overhead.
	runtimeBytes = 32 << 20
)

// LivePerViewerBytes is the live term per viewer (pion buffers). Measured
// by live.BenchmarkViewerMemory at about 2.2 MiB of heap and stacks per
// WHEP viewer (400 KiB/s H.264, default NACK and RTCP interceptors), rounded up.
var LivePerViewerBytes int64 = 3 << 20

// CameraBudget is one camera's share of the memory budget, in bytes.
type CameraBudget struct {
	ID     string
	Ring   int64
	Active int64
	Queues int64
	Total  int64
}

// BudgetReport is the computed memory budget, in bytes. Limit is 80% of
// relay.max_memory_mb and OK reports Total <= Limit.
type BudgetReport struct {
	Cameras      []CameraBudget
	LiveBytes    int64
	BacklogBytes int64
	RuntimeBytes int64
	Total        int64
	Limit        int64
	OK           bool
}

// Budget computes the memory budget of c (design §8.1). Per camera, with
// B = max_bitrate_kbps × 1000 / 8 bytes/s and G = max_gop_seconds:
//
//	ring   = B × (pre_roll_seconds + G) × 1.25
//	active = 2 × chunk_size + B × G + part_size_mib MiB
//	queues = (recorder_queue_frames + viewer_queue_frames × live.max_viewers) × B / fps_est
//
// and total = Σ cameras + live.max_viewers × LivePerViewerBytes +
// egress_backlog_mb MiB + 32 MiB. Every product is taken before its
// integer division.
func Budget(c *Config) BudgetReport {
	viewers := int64(c.Live.MaxViewers)
	chunk := int64(c.Encryption.ChunkSizeKiB) << 10
	part := int64(c.Storage.S3.PartSizeMiB) * mib
	secs := func(d time.Duration) int64 { return int64(d / time.Second) }

	r := BudgetReport{
		LiveBytes:    viewers * LivePerViewerBytes,
		BacklogBytes: int64(c.Relay.EgressBacklogMB) * mib,
		RuntimeBytes: runtimeBytes,
		Limit:        int64(c.Relay.MaxMemoryMB) * mib * 8 / 10,
	}
	r.Total = r.LiveBytes + r.BacklogBytes + r.RuntimeBytes
	for _, cam := range c.Cameras {
		b := int64(cam.MaxBitrateKbps) * 1000 / 8
		g := secs(cam.MaxGOP)
		cb := CameraBudget{
			ID:     cam.ID,
			Ring:   b * (secs(cam.PreRoll) + g) * 5 / 4,
			Active: 2*chunk + b*g + part,
			Queues: (int64(cam.RecorderQueueFrames) + int64(cam.ViewerQueueFrames)*viewers) * b / budgetFPSEstimate,
		}
		cb.Total = cb.Ring + cb.Active + cb.Queues
		r.Cameras = append(r.Cameras, cb)
		r.Total += cb.Total
	}
	r.OK = r.Total <= r.Limit
	return r
}

// KeyFingerprint parses the resolved master key of c and returns its
// key_id as shown by "config check" (a1b2-c3d4-e5f6-0718). Errors never
// carry the key text.
func KeyFingerprint(c *Config) (string, error) {
	m, err := zrec.ParseMasterKey(c.Encryption.MasterKeyText)
	if err != nil {
		return "", err
	}
	return zrec.FormatKeyID(zrec.Derive(m).ID), nil
}

// WriteTable prints the budget as a table of bytes and MiB, ending with
// the TOTAL row and the 80% limit.
func (r BudgetReport) WriteTable(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	mibs := func(n int64) string { return fmt.Sprintf("%.1f", float64(n)/float64(mib)) }
	fmt.Fprintln(tw, "ITEM\tRING\tACTIVE\tQUEUES\tBYTES\tMIB")
	for _, c := range r.Cameras {
		fmt.Fprintf(tw, "camera %s\t%d\t%d\t%d\t%d\t%s\n", c.ID, c.Ring, c.Active, c.Queues, c.Total, mibs(c.Total))
	}
	row := func(name string, n int64) {
		fmt.Fprintf(tw, "%s\t\t\t\t%d\t%s\n", name, n, mibs(n))
	}
	row("live", r.LiveBytes)
	row("backlog", r.BacklogBytes)
	row("runtime", r.RuntimeBytes)
	row("TOTAL", r.Total)
	row("limit (80% of max_memory_mb)", r.Limit)
	verdict := "OK"
	if !r.OK {
		verdict = "OVER"
	}
	fmt.Fprintf(tw, "budget\t\t\t\t\t%s\n", verdict)
	tw.Flush()
}
