// Package recorder glues the clip state machine to encryption and storage
// (relay-spec §4.5-4.7, Contract §7.3): it implements clip.Output, turning
// each clip segment into a muxed fMP4 stream, encrypted with a zrec writer
// and handed to a ClipSink, together with the staging and final sidecars.
//
// Plaintext exists only in the muxer's buffers and the zrec writer; the sink
// sees ciphertext and sidecar JSON.
package recorder

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/clip"
	"github.com/apikdech/zrelay-v2/internal/relay/config"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/relay/mux"
	"github.com/apikdech/zrelay-v2/internal/relay/ring"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// defaultChunkSize is the zrec writer's default plaintext chunk size
// (Contract §4.1), used when Config.ChunkSize is 0.
const defaultChunkSize = 1 << 20

// tickEvery is how often Run ticks the clip machine.
const tickEvery = 250 * time.Millisecond

// Config configures a Recorder.
type Config struct {
	Camera    config.Camera
	RelayID   string
	Version   string
	Keys      zrec.Keys
	ChunkSize int // plaintext chunk size; 0 selects the zrec default (1 MiB)
	Clock     clock.Clock
}

// PendingClip is what a ClipSink needs to store one clip.
//
// The recorder fills Keys, Staging and Final. The sink fills Done from Begin
// and the recorder calls it exactly once, after the returned writer is
// closed (nil) or abandoned (non-nil error).
type PendingClip struct {
	Keys sidecar.ClipKeySet
	// Staging is the staging sidecar JSON (Contract §7.3 step 0). It MUST be
	// stored before any byte of the video.
	Staging []byte
	// Final builds the final sidecar JSON. It is valid only once the writer
	// is closed, so a sink calls it from Done(nil).
	Final func() ([]byte, error)
	// Done is set by the sink in Begin. Done(nil) means the zrec stream is
	// complete: commit the video, store Final(), delete the staging sidecar.
	// Done(err) means the clip is abandoned: abort the video and delete the
	// staging sidecar. Done may block (it is the synchronous storage path).
	Done func(err error)
}

// ClipSink takes the encrypted stream of one clip. Begin stores c.Staging
// first, then returns the writer for the zrec bytes.
type ClipSink interface {
	Begin(c *PendingClip) (io.Writer, error)
}

// Recorder implements clip.Output.
type Recorder struct {
	cfg  Config
	sink ClipSink
}

var _ clip.Output = (*Recorder)(nil)

// New returns a Recorder that stores clips through sink.
func New(cfg Config, sink ClipSink) *Recorder {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real
	}
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = defaultChunkSize
	}
	return &Recorder{cfg: cfg, sink: sink}
}

// lateWriter holds the init segment in memory until the zrec writer exists:
// the staging sidecar needs init_segment_bytes, so the muxer runs first, but
// the staging sidecar must reach storage before the encrypted stream starts.
type lateWriter struct {
	buf bytes.Buffer
	dst io.Writer
}

func (l *lateWriter) Write(p []byte) (int, error) {
	if l.dst != nil {
		return l.dst.Write(p)
	}
	return l.buf.Write(p)
}

// attach forwards the buffered bytes to dst and wipes the buffer.
func (l *lateWriter) attach(dst io.Writer) error {
	b := l.buf.Bytes()
	_, err := dst.Write(b)
	clear(b)
	l.buf = bytes.Buffer{}
	l.dst = dst
	return err
}

// countWriter counts the ciphertext bytes handed to the sink.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Open starts one clip segment (clip.Output). The staging sidecar is stored
// before the first byte of the video.
func (r *Recorder) Open(o clip.OpenInfo) (clip.ClipWriter, error) {
	codec, err := mux.CodecString(o.Codec)
	if err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	late := &lateWriter{}
	mx, err := mux.New(late, o.Codec)
	if err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	keys := sidecar.ClipKeys(r.cfg.Camera.ID, o.Start, o.ClipID)
	w := &clipWriter{r: r, info: o, codec: codec, mx: mx, initBytes: mx.Stats().InitSegmentBytes}

	staging, err := json.Marshal(w.sidecar())
	if err != nil {
		return nil, fmt.Errorf("recorder: encode staging sidecar: %w", err)
	}
	w.pc = &PendingClip{Keys: keys, Staging: staging, Final: w.final}
	sw, err := r.sink.Begin(w.pc)
	if err != nil {
		return nil, fmt.Errorf("recorder: begin clip: %w", err)
	}
	w.out = &countWriter{w: sw}
	w.zw, err = zrec.NewWriter(w.out, zrec.WriterParams{
		Keys: r.cfg.Keys, ClipID: o.ClipID, StartUnixMs: o.Start.UnixMilli(),
		CameraID: r.cfg.Camera.ID, ChunkSize: r.cfg.ChunkSize,
	})
	if err != nil {
		w.done(err)
		return nil, fmt.Errorf("recorder: %w", err)
	}
	if err := late.attach(w.zw); err != nil {
		w.zw.Abort()
		w.done(err)
		return nil, fmt.Errorf("recorder: write init segment: %w", err)
	}
	return w, nil
}

// clipWriter is one open clip segment (clip.ClipWriter).
type clipWriter struct {
	r         *Recorder
	info      clip.OpenInfo
	codec     string
	initBytes int64
	mx        *mux.Muxer
	zw        *zrec.Writer
	out       *countWriter
	pc        *PendingClip

	fin   clip.FinalInfo
	ended bool
}

var errEnded = errors.New("recorder: clip already ended")

// sidecar returns the staging form of the clip's sidecar: every field known
// at open, marked incomplete as crash recovery would publish it (Contract
// §7.3 step 0, §7.4). Recovery sets size_bytes from the stored object.
func (w *clipWriter) sidecar() *sidecar.Sidecar {
	cfg := w.r.cfg
	o := w.info
	tags := cfg.Camera.Tags
	if tags == nil {
		tags = []string{}
	}
	return &sidecar.Sidecar{
		Schema:       sidecar.Schema,
		Status:       sidecar.StatusIncomplete,
		ClipID:       o.ClipID,
		SeriesID:     o.SeriesID,
		SegmentIndex: o.SegmentIndex,
		Camera:       sidecar.Camera{ID: cfg.Camera.ID, Name: cfg.Camera.Name, Brand: cfg.Camera.Brand, Tags: tags},
		StartTime:    sidecar.Time{Time: o.Start.UTC()},
		PreRollMs:    o.PreRollMs,
		EndReason:    string(clip.ReasonRecovered),
		Triggers:     []sidecar.Trigger{},
		Video: sidecar.Video{
			Object:           sidecar.ObjectName(o.Start, o.ClipID, "zrec"),
			FormatVersion:    1,
			KeyID:            hex.EncodeToString(cfg.Keys.ID[:]),
			ChunkSize:        int64(cfg.ChunkSize),
			Container:        "fmp4",
			Codec:            w.codec,
			Width:            o.Codec.Width,
			Height:           o.Codec.Height,
			InitSegmentBytes: w.initBytes,
		},
		Recorder: sidecar.Recorder{RelayID: cfg.RelayID, Version: cfg.Version, TimeSource: string(o.TimeSource)},
	}
}

// final builds the final sidecar JSON; it is valid after Finalize closed the
// writer.
func (w *clipWriter) final() ([]byte, error) {
	if !w.ended || w.out == nil {
		return nil, errors.New("recorder: final sidecar requested before the clip was finalized")
	}
	st := w.mx.Stats()
	sc := w.sidecar()
	f := w.fin
	end := sidecar.Time{Time: f.End.UTC()}
	sc.Status = sidecar.StatusComplete
	sc.EndReason = string(f.Reason)
	sc.EndTime = &end
	sc.DurationMs = &f.DurationMs
	for _, t := range f.Triggers {
		sc.Triggers = append(sc.Triggers, sidecar.Trigger{
			Type: string(t.Kind), FirstAtMs: t.FirstAtMs, LastAtMs: t.LastAtMs,
			RawTopics: slices.Clone(t.RawTopics),
		})
	}
	seek := st.SeekIndex
	if seek == nil {
		seek = [][2]int64{}
	}
	sc.Video.SizeBytes = w.out.n
	sc.Video.PlaintextSizeBytes = &st.PlaintextBytes
	sc.Video.SeekIndex = &seek
	return json.Marshal(sc)
}

func (w *clipWriter) done(err error) {
	if w.pc.Done != nil {
		w.pc.Done(err)
	}
}

// WriteGOP muxes g into the encrypted stream.
func (w *clipWriter) WriteGOP(g *ring.GOP) error {
	if w.ended {
		return errEnded
	}
	return w.mx.WriteGOP(g)
}

// Finalize seals the stream and hands the clip to the sink to commit.
func (w *clipWriter) Finalize(f clip.FinalInfo) error {
	if w.ended {
		return errEnded
	}
	w.ended = true
	w.fin = f
	if err := w.zw.Close(); err != nil {
		w.zw.Abort()
		w.done(err)
		return fmt.Errorf("recorder: close stream: %w", err)
	}
	w.done(nil)
	return nil
}

// Abort discards the clip: the DEK is zeroed and the sink drops the object
// and the staging sidecar.
func (w *clipWriter) Abort(reason clip.EndReason) {
	if w.ended {
		return
	}
	w.ended = true
	w.zw.Abort()
	w.done(fmt.Errorf("recorder: clip aborted (%s)", reason))
}

// DirectSink stores a clip synchronously through a storage.Writer
// (Contract §7.3): staging sidecar, then the video object, and on completion
// commit, final sidecar, delete staging. Phase 6 replaces it with egress.
type DirectSink struct {
	W storage.Writer
}

var _ ClipSink = DirectSink{}

// Begin stores the staging sidecar, creates the video object and arranges
// c.Done to commit or abort it.
func (d DirectSink) Begin(c *PendingClip) (io.Writer, error) {
	ctx := context.Background()
	if err := d.W.PutSmall(ctx, c.Keys.StagingSidecar, c.Staging, "application/json"); err != nil {
		return nil, fmt.Errorf("staging sidecar: %w", err)
	}
	ow, err := d.W.Create(ctx, c.Keys.Video)
	if err != nil {
		d.dropStaging(ctx, c)
		return nil, fmt.Errorf("create video: %w", err)
	}
	c.Done = func(err error) {
		if err != nil {
			d.abandon(ctx, c, ow)
			return
		}
		if err := ow.Commit(ctx); err != nil {
			slog.Warn("recorder: commit failed", "key", c.Keys.Video, "error", err)
			d.abandon(ctx, c, ow)
			return
		}
		b, err := c.Final()
		if err == nil {
			err = d.W.PutSmall(ctx, c.Keys.Sidecar, b, "application/json")
		}
		if err != nil {
			// The staging sidecar stays: crash recovery promotes it.
			slog.Warn("recorder: final sidecar not stored", "key", c.Keys.Sidecar, "error", err)
			return
		}
		d.dropStaging(ctx, c)
	}
	return ow, nil
}

func (d DirectSink) abandon(ctx context.Context, c *PendingClip, ow storage.ObjectWriter) {
	if err := ow.Abort(ctx); err != nil {
		slog.Warn("recorder: abort failed", "key", c.Keys.Video, "error", err)
	}
	d.dropStaging(ctx, c)
}

func (d DirectSink) dropStaging(ctx context.Context, c *PendingClip) {
	if err := d.W.Delete(ctx, c.Keys.StagingSidecar); err != nil && !errors.Is(err, storage.ErrNotFound) {
		slog.Warn("recorder: staging sidecar not deleted", "key", c.Keys.StagingSidecar, "error", err)
	}
}

// RunOption configures Run.
type RunOption func(*runOptions)

type runOptions struct {
	force      <-chan struct{}
	resume     func() bool
	suppressed func(bool)
}

// WithSuppressionHook reports every time the clip machine enters (true) or
// leaves (false) the Suppressed state, from the recorder goroutine. A
// suppressed camera ignores triggers, so coverage must not list that time as
// monitored. The state is sampled after every input the loop handles, which
// is how a write error inside OnVideo is seen too.
func WithSuppressionHook(f func(suppressed bool)) RunOption {
	return func(o *runOptions) { o.suppressed = f }
}

// WithBacklogPolicy lets the egress backlog policy (relay-spec §4.8), which
// runs on other goroutines, act on the camera from the recorder goroutine:
// a receive on force finalizes the active clip with backlog_overflow and
// suppresses the camera, and on every tick a Suppressed camera is released
// when resume reports true (backlog < 50% of the limit, relay-spec §4.5).
func WithBacklogPolicy(force <-chan struct{}, resume func() bool) RunOption {
	return func(o *runOptions) { o.force, o.resume = force, resume }
}

// Run is the per-camera recorder loop: it feeds Hub events and tracker
// changes to the clip machine and ticks it every 250 ms, until ctx is done or
// sub is closed. It then shuts the machine down (finalizing any open clip
// with end_reason shutdown) and closes sub.
//
// The tracker reports only which kinds are active, so the trigger signals the
// machine records carry host receive time and no raw topic.
func Run(ctx context.Context, sub *camera.Sub, tracker *events.Tracker, m *clip.Machine, opts ...RunOption) {
	defer sub.Close()
	defer m.Shutdown()
	var ro runOptions
	for _, o := range opts {
		o(&ro)
	}

	active := map[events.Kind]bool{}
	syncTriggers := func() {
		now := time.Now().UTC()
		cur := tracker.Active()
		for _, k := range cur {
			if !active[k] {
				active[k] = true
				m.OnSignal(events.Signal{Kind: k, Active: true, At: now})
			}
		}
		for k := range active {
			if !slices.Contains(cur, k) {
				delete(active, k)
				m.OnSignal(events.Signal{Kind: k, Active: false, At: now})
			}
		}
		m.OnTriggered(len(cur) > 0)
	}
	syncTriggers()

	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	wasSuppressed := false
	for {
		if ro.suppressed != nil {
			if now := m.State() == "Suppressed"; now != wasSuppressed {
				wasSuppressed = now
				ro.suppressed(now)
			}
		}
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C():
			if !ok {
				return
			}
			switch ev.Kind {
			case camera.EventCodec:
				m.OnCodec(*ev.Codec, ev.TimeSource)
			case camera.EventVideo:
				m.OnVideo(ev.AU)
			case camera.EventLost:
				m.OnLost()
			}
		case <-tracker.Changes():
			syncTriggers()
		case <-ro.force:
			m.ForceFinalize(clip.ReasonBacklogOverflow)
		case <-ticker.C:
			m.Tick()
			if ro.resume != nil && m.State() == "Suppressed" && ro.resume() {
				m.Unsuppress()
			}
		}
	}
}
