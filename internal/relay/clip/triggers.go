package clip

import (
	"cmp"
	"slices"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
)

// TriggerRecord is the sidecar's per-kind trigger summary (Contract §6.2):
// offsets in milliseconds from the clip start, and the vendor topics that
// matched, deduplicated in order of first appearance.
type TriggerRecord struct {
	Kind                events.Kind
	FirstAtMs, LastAtMs int64
	RawTopics           []string
}

// trigRec accumulates the signals of one kind.
type trigRec struct {
	first, last time.Time
	active      bool
	topics      []string
}

// trigSet is the trigger bookkeeping of a camera. It follows the signals
// regardless of the clip state, so a kind that was already active when a clip
// opens, or that stays active across a segment split, is recorded.
type trigSet struct {
	byKind map[events.Kind]*trigRec
}

// apply folds one signal in. When no clip is open (recording false), a kind
// that ends is forgotten: it never produced a clip.
func (t *trigSet) apply(s events.Signal, recording bool) {
	rec := t.byKind[s.Kind]
	if !s.Active {
		if rec == nil {
			return
		}
		if !recording {
			delete(t.byKind, s.Kind)
			return
		}
		rec.active = false
		rec.touch(s)
		return
	}
	if rec == nil {
		if t.byKind == nil {
			t.byKind = make(map[events.Kind]*trigRec)
		}
		rec = &trigRec{first: s.At, last: s.At}
		t.byKind[s.Kind] = rec
	}
	rec.active = true
	rec.touch(s)
}

func (r *trigRec) touch(s events.Signal) {
	if s.At.Before(r.first) {
		r.first = s.At
	}
	if s.At.After(r.last) {
		r.last = s.At
	}
	if s.RawTopic != "" && !slices.Contains(r.topics, s.RawTopic) {
		r.topics = append(r.topics, s.RawTopic)
	}
}

// snapshot returns the records of a clip that starts at start and lasts
// durMs. Offsets are clamped to [0, durMs] (relay-spec §4.5, clock mixing).
// A kind still active ends with the clip.
func (t *trigSet) snapshot(start time.Time, durMs int64) []TriggerRecord {
	out := make([]TriggerRecord, 0, len(t.byKind))
	off := func(at time.Time) int64 {
		return min(max(at.Sub(start).Milliseconds(), 0), durMs)
	}
	for kind, rec := range t.byKind {
		tr := TriggerRecord{
			Kind:      kind,
			FirstAtMs: off(rec.first),
			LastAtMs:  off(rec.last),
			RawTopics: slices.Clone(rec.topics),
		}
		if rec.active {
			tr.LastAtMs = durMs
		}
		out = append(out, tr)
	}
	slices.SortFunc(out, func(a, b TriggerRecord) int {
		return cmp.Or(cmp.Compare(a.FirstAtMs, b.FirstAtMs), cmp.Compare(a.Kind, b.Kind))
	})
	return out
}

// endClip drops the kinds that ended during the finished clip. Kinds still
// active carry on into the next segment.
func (t *trigSet) endClip() {
	for kind, rec := range t.byKind {
		if !rec.active {
			delete(t.byKind, kind)
		}
	}
}
