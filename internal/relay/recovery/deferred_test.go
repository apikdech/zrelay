package recovery_test

import (
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/recovery"
)

// TestRunDetailedCountsYoungCandidatesAsDeferred: a crashed clip younger than
// the age filter is left alone, but the caller must be told, so that it scans
// again once the candidate has aged. A recovery that counts such a run as
// done never revisits a clip crashed seconds before the restart.
func TestRunDetailedCountsYoungCandidatesAsDeferred(t *testing.T) {
	e := newLocalEnv(t)
	both := newClip(t, camera, hourAgo(), 30)
	alone := newClip(t, camera, hourAgo().Add(time.Second), 31)
	e.putStaging(t, both, camera)
	e.crashVideo(t, both, camera, false)
	e.putStaging(t, alone, camera)

	res, err := recovery.RunDetailed(t.Context(), recovery.Config{
		Provider: e.p, Recoverer: e.r, Cameras: []string{camera},
		Now: time.Now().UTC(), OlderThan: time.Hour,
	})
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	wantReport(t, res.Report, recovery.Report{})
	if res.Deferred != 2 {
		t.Errorf("Deferred = %d, want 2 (the interrupted video with its staging sidecar, and the lone staging sidecar)", res.Deferred)
	}

	// Once aged, the same candidates are handled and nothing is deferred.
	e.age(t, both.keys.Video, both.keys.StagingSidecar, alone.keys.StagingSidecar)
	res, err = recovery.RunDetailed(t.Context(), recovery.Config{
		Provider: e.p, Recoverer: e.r, Cameras: []string{camera},
		Now: time.Now().UTC(), OlderThan: time.Minute,
	})
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	if res.Deferred != 0 || res.Recovered != 1 {
		t.Errorf("result after ageing = %+v, want one recovered and none deferred", res)
	}
}

// TestRunLeavesCandidatesFromThisProcessAlone: Before is the start of the
// running process. Candidates last modified at or after it belong to this
// process's own writers (however old the reference time makes them look), and
// so do the keys InFlight names. Neither is touched nor deferred.
func TestRunLeavesCandidatesFromThisProcessAlone(t *testing.T) {
	e := newLocalEnv(t)
	crashed := newClip(t, camera, hourAgo(), 32)
	e.putStaging(t, crashed, camera)
	e.crashVideo(t, crashed, camera, false)
	own := newClip(t, camera, hourAgo().Add(time.Second), 33)
	e.putStaging(t, own, camera)
	e.crashVideo(t, own, camera, false)
	e.age(t, crashed.keys.Video, crashed.keys.StagingSidecar)
	// The process started a minute ago: the crashed clip (aged to ten
	// minutes ago) is before it, own, written just now, is after it.
	startedAt := time.Now().UTC().Add(-time.Minute)

	cfg := recovery.Config{
		Provider: e.p, Recoverer: e.r, Cameras: []string{camera},
		Now: time.Now().UTC().Add(time.Hour), OlderThan: time.Minute,
		Before: startedAt,
	}
	res, err := recovery.RunDetailed(t.Context(), cfg)
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	if res.Recovered != 1 || res.Deferred != 0 {
		t.Errorf("result = %+v, want only the crashed clip recovered", res)
	}
	if !e.exists(t, crashed.keys.Sidecar) || e.exists(t, own.keys.Sidecar) {
		t.Error("recovery did not separate the crashed clip from this process's own")
	}
	if got := e.staged(t); len(got) != 1 || got[0] != own.keys.StagingSidecar {
		t.Errorf("staging = %q, want only the own clip's left", got)
	}

	// InFlight protects a key even when its time says it is old.
	cfg.Before = time.Time{}
	cfg.InFlight = func(key string) bool { return key == own.keys.Video || key == own.keys.StagingSidecar }
	res, err = recovery.RunDetailed(t.Context(), cfg)
	if err != nil {
		t.Fatalf("RunDetailed: %v", err)
	}
	if res.Report != (recovery.Report{}) || res.Deferred != 0 || e.exists(t, own.keys.Sidecar) {
		t.Errorf("an in-flight clip was touched: %+v", res)
	}
}
