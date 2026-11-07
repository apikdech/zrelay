// Package recovery performs the relay's startup crash recovery (Contract
// §7.4, design §6.5): it finds clips interrupted by a crash under today's and
// yesterday's prefixes, pairs each interrupted video with its staging sidecar
// (Contract §7.3 step 0) and publishes, promotes or discards them.
//
// Recovery never reads the stream's plaintext and never needs the master key:
// it reads only the plaintext zrec descriptor (zrec.ParseHeader).
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
	"github.com/apikdech/zrelay-v2/pkg/zrec"
)

// DefaultOlderThan is the minimum age of a recovery candidate (Contract
// §7.4). Config.OlderThan of zero selects it.
const DefaultOlderThan = 2 * time.Minute

const (
	stagingSuffix = ".json.part"
	videoExt      = ".zrec"
	sidecarExt    = ".json"
)

// Config configures Run.
type Config struct {
	Provider  storage.Provider
	Recoverer storage.Recoverer
	// Cameras lists the camera ids whose clips are scanned.
	Cameras []string
	// Now selects the two UTC days scanned (Now's day and the day before) and
	// is the reference for the age of staging sidecars.
	Now time.Time
	// OlderThan is the minimum age of a candidate; zero means DefaultOlderThan.
	OlderThan time.Duration
}

// Report counts what Run did, one count per clip.
type Report struct {
	// Recovered clips had an interrupted video and a staging sidecar: the video
	// was committed and an incomplete sidecar published.
	Recovered int
	// Promoted clips had a committed video and a staging sidecar but no real
	// sidecar: the staging sidecar was promoted to an incomplete sidecar.
	Promoted int
	// Discarded counts interrupted videos without a staging sidecar, pairs
	// whose descriptor was unreadable, and staging sidecars that had nothing
	// left to describe or whose clip already completed.
	Discarded int
	// Orphaned counts committed videos left in place without a sidecar because
	// the descriptor was unreadable after commit or did not match the staging
	// sidecar. They are invisible (Contract §7.3) and expire by ILM (design §6.5).
	Orphaned int
}

// aborter is implemented by drivers that can abort a candidate directly (s3).
type aborter interface {
	AbortIncomplete(ctx context.Context, inc storage.Incomplete) error
}

type runner struct {
	cfg       Config
	olderThan time.Duration
	rep       Report
	errs      []error
	// pending counts, per final video key, the candidate uploads not yet
	// concluded. Several uploads can exist for one key (a lost
	// CreateMultipartUpload response, then a retry); they share one staging
	// sidecar, which must outlive all of them.
	pending map[string]int
	// settled holds the staging sidecars already deleted.
	settled map[string]bool
}

// Run performs startup recovery for every configured camera. A failure on one
// clip is recorded and the remaining clips are still processed; the returned
// error joins all of them (nil when everything was handled), and the Report
// counts what was done. Run is idempotent: a clip handled once leaves nothing
// for a second Run, and an interrupted Run is completed by the next.
func Run(ctx context.Context, cfg Config) (Report, error) {
	if cfg.Provider == nil || cfg.Recoverer == nil {
		return Report{}, errors.New("recovery: Provider and Recoverer are required")
	}
	if cfg.Now.IsZero() {
		return Report{}, errors.New("recovery: Now is required")
	}
	r := &runner{cfg: cfg, olderThan: cfg.OlderThan, pending: map[string]int{}, settled: map[string]bool{}}
	if r.olderThan == 0 {
		r.olderThan = DefaultOlderThan
	}
	now := cfg.Now.UTC()
	days := []time.Time{now, now.AddDate(0, 0, -1)}
	for _, cam := range cfg.Cameras {
		for _, day := range days {
			if err := ctx.Err(); err != nil {
				return r.rep, errors.Join(append(r.errs, err)...)
			}
			if err := r.scan(ctx, sidecar.DayPrefix(cam, day)); err != nil {
				r.errs = append(r.errs, err)
			}
		}
	}
	return r.rep, errors.Join(r.errs...)
}

// staged is one staging sidecar and the keys derived from it.
type staged struct {
	info    storage.ObjectInfo
	video   string // <dir>/<base>.zrec
	sidecar string // <dir>/<base>.json
}

// stagedKeys derives the video and real sidecar keys from a staging key
// "<dir>/.<base>.json.part", where <base>.json is a valid object name.
func stagedKeys(info storage.ObjectInfo) (staged, bool) {
	dir, name := path.Split(info.Key)
	base, ok := strings.CutSuffix(name, stagingSuffix)
	if !ok || !strings.HasPrefix(base, ".") {
		return staged{}, false
	}
	base = base[1:]
	if !sidecar.NameRE.MatchString(base + sidecarExt) {
		return staged{}, false
	}
	return staged{info: info, video: dir + base + videoExt, sidecar: dir + base + sidecarExt}, true
}

func (r *runner) fail(err error) { r.errs = append(r.errs, err) }

func (r *runner) scan(ctx context.Context, prefix string) error {
	infos, err := r.cfg.Recoverer.ListStaging(ctx, prefix)
	if err != nil {
		return fmt.Errorf("recovery: list staging under %q: %w", prefix, err)
	}
	byVideo := map[string]staged{}
	for _, info := range infos {
		if s, ok := stagedKeys(info); ok {
			byVideo[s.video] = s
		}
	}
	// Every incomplete is listed regardless of age; the age filter is applied
	// here against cfg.Now, the same reference as for staging sidecars. A
	// young incomplete must still be seen, so that its staging sidecar is not
	// mistaken for an unpaired one.
	incs, err := r.cfg.Recoverer.ListIncomplete(ctx, prefix, 0)
	if err != nil {
		return fmt.Errorf("recovery: list incomplete under %q: %w", prefix, err)
	}

	paired := map[string]bool{}
	for _, inc := range incs {
		r.pending[inc.Key]++
	}
	for _, inc := range incs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(inc.Key, videoExt) || !sidecar.NameRE.MatchString(path.Base(inc.Key)) {
			continue // not a clip this relay wrote
		}
		s, hasStaging := byVideo[inc.Key]
		if hasStaging {
			paired[inc.Key] = true // handled, even if left alone below
		}
		if r.cfg.Now.Sub(inc.ModTime) < r.olderThan {
			continue // too young: the clip may still be being written
		}
		if hasStaging {
			if err := r.recoverPair(ctx, inc, s); err != nil {
				r.fail(err)
			} else {
				r.pending[inc.Key]--
			}
			continue
		}
		if err := r.discardIncomplete(ctx, inc); err != nil {
			r.fail(err)
		} else {
			r.pending[inc.Key]--
		}
	}

	for video, s := range byVideo {
		if paired[video] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.cfg.Now.Sub(s.info.ModTime) < r.olderThan {
			continue // too young: may belong to a clip still being written
		}
		if err := r.unpaired(ctx, s); err != nil {
			r.fail(err)
		}
	}
	return nil
}

// discardIncomplete drops an interrupted video that has no staging sidecar:
// by the §7.3 ordering invariant it holds nothing playable.
func (r *runner) discardIncomplete(ctx context.Context, inc storage.Incomplete) error {
	var err error
	switch a, ok := r.cfg.Recoverer.(aborter); {
	case ok:
		err = a.AbortIncomplete(ctx, inc)
	case inc.StagingKey != "":
		err = r.cfg.Provider.Delete(ctx, inc.StagingKey)
	default:
		err = errors.New("driver cannot abort this candidate")
	}
	if err != nil {
		return fmt.Errorf("recovery: discard %q: %w", inc.Key, err)
	}
	r.rep.Discarded++
	return nil
}

// recoverPair handles a staging sidecar and an interrupted video together
// (Contract §7.4 triage, row 1).
func (r *runner) recoverPair(ctx context.Context, inc storage.Incomplete, s staged) error {
	var hdr zrec.Header
	if inc.StagingKey != "" {
		// The driver stages in a readable file: validate the descriptor before
		// committing anything (Contract §7.4, "A .part with no readable
		// descriptor").
		h, err := r.readHeader(ctx, inc.StagingKey)
		if err != nil {
			if isFormat(err) {
				return r.discardPair(ctx, inc, s)
			}
			return err
		}
		hdr = h
	}
	info, err := r.cfg.Recoverer.RecoverCommit(ctx, inc)
	if errors.Is(err, storage.ErrNothingToRecover) {
		// The driver found no durable byte, so no descriptor either. Another
		// upload for the same key may still hold the clip's data and need the
		// staging sidecar (or have used it already): only the last candidate
		// to conclude may delete it.
		if r.settled[s.info.Key] || r.pending[inc.Key] > 1 {
			return nil
		}
		return r.deleteStaging(ctx, s, &r.rep.Discarded)
	}
	if err != nil {
		return fmt.Errorf("recovery: commit %q: %w", inc.Key, err)
	}
	if inc.StagingKey == "" {
		// s3: an in-progress upload cannot be read, so the descriptor is read
		// from the committed object (design §6.5).
		h, err := r.readHeader(ctx, inc.Key)
		if err != nil {
			if isFormat(err) {
				return r.deleteStaging(ctx, s, &r.rep.Orphaned)
			}
			return err
		}
		hdr = h
	}
	return r.publish(ctx, s, hdr, info.Size, &r.rep.Recovered)
}

// discardPair removes an interrupted local video and its staging sidecar.
func (r *runner) discardPair(ctx context.Context, inc storage.Incomplete, s staged) error {
	if err := r.cfg.Provider.Delete(ctx, inc.StagingKey); err != nil {
		return fmt.Errorf("recovery: discard %q: %w", inc.Key, err)
	}
	return r.deleteStaging(ctx, s, &r.rep.Discarded)
}

// unpaired decides a staging sidecar with no interrupted video by what
// survives (Contract §7.4, second table).
func (r *runner) unpaired(ctx context.Context, s staged) error {
	p := r.cfg.Provider
	if _, err := p.Stat(ctx, s.sidecar); err == nil {
		return r.deleteStaging(ctx, s, &r.rep.Discarded) // the clip completed
	} else if !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("recovery: stat %q: %w", s.sidecar, err)
	}
	vi, err := p.Stat(ctx, s.video)
	if errors.Is(err, storage.ErrNotFound) {
		return r.deleteStaging(ctx, s, &r.rep.Discarded) // the discard was interrupted
	}
	if err != nil {
		return fmt.Errorf("recovery: stat %q: %w", s.video, err)
	}
	hdr, err := r.readHeader(ctx, s.video)
	if err != nil {
		if isFormat(err) {
			return r.deleteStaging(ctx, s, &r.rep.Orphaned)
		}
		return err
	}
	return r.publish(ctx, s, hdr, vi.Size, &r.rep.Promoted)
}

// publish cross-checks the staging sidecar against the descriptor, writes the
// incomplete sidecar and deletes the staging sidecar. On a mismatch the
// descriptor wins (Contract §7.4): the video stays without a sidecar.
func (r *runner) publish(ctx context.Context, s staged, hdr zrec.Header, size int64, counter *int) error {
	sc, err := r.buildSidecar(ctx, s, hdr, size)
	if err != nil {
		if isTransient(err) {
			return err
		}
		return r.deleteStaging(ctx, s, &r.rep.Orphaned)
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("recovery: encode sidecar %q: %w", s.sidecar, err)
	}
	if err := r.cfg.Provider.PutSmall(ctx, s.sidecar, b, "application/json"); err != nil {
		return fmt.Errorf("recovery: write sidecar %q: %w", s.sidecar, err)
	}
	return r.deleteStaging(ctx, s, counter)
}

func (r *runner) deleteStaging(ctx context.Context, s staged, counter *int) error {
	if err := r.cfg.Provider.Delete(ctx, s.info.Key); err != nil {
		return fmt.Errorf("recovery: delete staging sidecar %q: %w", s.info.Key, err)
	}
	r.settled[s.info.Key] = true
	*counter++
	return nil
}

// errMismatch marks a staging sidecar that is unusable or does not describe
// the video.
type errMismatch struct{ reason string }

func (e *errMismatch) Error() string { return "recovery: staging sidecar mismatch: " + e.reason }

func isTransient(err error) bool {
	var m *errMismatch
	return !errors.As(err, &m)
}

// buildSidecar turns the staging sidecar into the recovered sidecar
// (Contract §7.4). It returns *errMismatch when the staging sidecar is
// unusable or its clip_id, camera id, start_time or video object differ from
// the descriptor.
func (r *runner) buildSidecar(ctx context.Context, s staged, hdr zrec.Header, size int64) (*sidecar.Sidecar, error) {
	raw, err := readAll(ctx, r.cfg.Provider, s.info.Key, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("recovery: read staging sidecar %q: %w", s.info.Key, err)
	}
	raw, err = withSchema(raw)
	if err != nil {
		return nil, &errMismatch{reason: err.Error()}
	}
	sc, err := sidecar.Decode(raw)
	if err != nil {
		return nil, &errMismatch{reason: err.Error()}
	}
	switch {
	case sc.ClipID != hdr.ClipID:
		return nil, &errMismatch{reason: "clip_id"}
	case sc.Camera.ID != hdr.CameraID:
		return nil, &errMismatch{reason: "camera.id"}
	case sc.StartTime.UnixMilli() != hdr.StartUnixMs:
		return nil, &errMismatch{reason: "start_time"}
	case sc.Video.Object != path.Base(s.video):
		return nil, &errMismatch{reason: "video.object"}
	case sidecar.ClipKeys(hdr.CameraID, time.UnixMilli(hdr.StartUnixMs), hdr.ClipID).Video != s.video:
		return nil, &errMismatch{reason: "key does not follow from the descriptor"}
	}
	sc.Status = sidecar.StatusIncomplete
	sc.EndReason = "recovered"
	sc.EndTime = nil
	sc.DurationMs = nil
	sc.Triggers = []sidecar.Trigger{}
	sc.Video.SizeBytes = size
	sc.Video.PlaintextSizeBytes = nil
	sc.Video.SeekIndex = nil
	if err := sc.Validate(); err != nil {
		return nil, &errMismatch{reason: err.Error()}
	}
	return sc, nil
}

// withSchema fills in "schema" when the staging sidecar lacks it. Contract
// §7.3 step 0 does not list it among the crash-lost state, and it is a
// constant, so recovery supplies it rather than orphaning the clip.
func withSchema(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if s, ok := doc["schema"]; ok && string(s) != "null" {
		return raw, nil
	}
	b, err := json.Marshal(sidecar.Schema)
	if err != nil {
		return nil, err
	}
	doc["schema"] = b
	return json.Marshal(doc)
}

// errDescriptor marks a descriptor that could not be parsed.
type errDescriptor struct{ err error }

func (e *errDescriptor) Error() string { return "recovery: unreadable descriptor: " + e.err.Error() }
func (e *errDescriptor) Unwrap() error { return e.err }

func isFormat(err error) bool {
	var d *errDescriptor
	return errors.As(err, &d)
}

// readHeader reads and parses the zrec descriptor of key. A short or
// malformed header is an *errDescriptor; a failed read is returned as is.
func (r *runner) readHeader(ctx context.Context, key string) (zrec.Header, error) {
	b, err := readAll(ctx, r.cfg.Provider, key, zrec.MaxHeaderLen)
	if err != nil {
		return zrec.Header{}, fmt.Errorf("recovery: read descriptor of %q: %w", key, err)
	}
	h, err := zrec.ParseHeader(b)
	if err != nil {
		return zrec.Header{}, &errDescriptor{err}
	}
	return h, nil
}

// readAll reads at most n bytes from the start of key.
func readAll(ctx context.Context, p storage.Reader, key string, n int64) ([]byte, error) {
	or, err := p.OpenRange(ctx, key, 0, n)
	if err != nil {
		return nil, err
	}
	defer or.Close()
	return io.ReadAll(io.LimitReader(or, n))
}
