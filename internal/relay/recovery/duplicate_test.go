package recovery_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/recovery"
	"github.com/apikdech/zrelay-v2/pkg/sidecar"
	"github.com/apikdech/zrelay-v2/pkg/storage"
)

// dupRecoverer reproduces a lost CreateMultipartUpload response: for every
// real candidate it lists a second, zero-part upload for the same final key,
// which RecoverCommit aborts with ErrNothingToRecover.
type dupRecoverer struct {
	storage.Recoverer
	orphanFirst bool
	failReal    bool
	aborted     int
}

const orphanUploadID = "orphan-upload"

func (d *dupRecoverer) ListIncomplete(ctx context.Context, prefix string, olderThan time.Duration) ([]storage.Incomplete, error) {
	real, err := d.Recoverer.ListIncomplete(ctx, prefix, olderThan)
	if err != nil {
		return nil, err
	}
	var out []storage.Incomplete
	for _, inc := range real {
		orphan := storage.Incomplete{Key: inc.Key, UploadID: orphanUploadID, ModTime: inc.ModTime}
		if d.orphanFirst {
			out = append(out, orphan, inc)
		} else {
			out = append(out, inc, orphan)
		}
	}
	return out, nil
}

func (d *dupRecoverer) RecoverCommit(ctx context.Context, inc storage.Incomplete) (storage.ObjectInfo, error) {
	if inc.UploadID == orphanUploadID {
		d.aborted++
		return storage.ObjectInfo{}, storage.ErrNothingToRecover
	}
	if d.failReal {
		return storage.ObjectInfo{}, errors.New("transient commit failure")
	}
	return d.Recoverer.RecoverCommit(ctx, inc)
}

// TestDuplicateUploadsKeepStagingForTheRealOne: two uploads for one video key
// (a lost CreateMultipartUpload response, then a crash). Aborting the empty
// one must not delete the staging sidecar the other still needs.
func TestDuplicateUploadsKeepStagingForTheRealOne(t *testing.T) {
	for _, orphanFirst := range []bool{true, false} {
		name := "OrphanLast"
		if orphanFirst {
			name = "OrphanFirst"
		}
		t.Run(name, func(t *testing.T) {
			e := newLocalEnv(t)
			d := &dupRecoverer{Recoverer: e.r, orphanFirst: orphanFirst}
			e.r = d
			c := newClip(t, camera, hourAgo(), 30)
			e.putStaging(t, c, camera)
			e.crashVideo(t, c, camera, false)
			e.age(t, c.keys.Video, c.keys.StagingSidecar)

			rep, err := e.run(t, camera)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			wantReport(t, rep, recovery.Report{Recovered: 1})
			if d.aborted != 1 {
				t.Errorf("empty upload aborted %d times, want 1", d.aborted)
			}
			sc, _ := e.readSidecar(t, c.keys.Sidecar)
			if sc.Status != sidecar.StatusIncomplete || sc.EndReason != "recovered" {
				t.Errorf("status/end_reason = %q/%q, want incomplete/recovered", sc.Status, sc.EndReason)
			}
			if got := e.staged(t); len(got) != 0 {
				t.Errorf("staging sidecars left: %q", got)
			}
		})
	}
}

// TestDuplicateUploadsKeepStagingAfterTransientFailure: the real upload could
// not be committed this time; the empty one being aborted afterwards must not
// delete the staging sidecar the retry needs.
func TestDuplicateUploadsKeepStagingAfterTransientFailure(t *testing.T) {
	e := newLocalEnv(t)
	d := &dupRecoverer{Recoverer: e.r, failReal: true}
	e.r = d
	c := newClip(t, camera, hourAgo(), 31)
	e.putStaging(t, c, camera)
	e.crashVideo(t, c, camera, false)
	e.age(t, c.keys.Video, c.keys.StagingSidecar)

	if _, err := e.run(t, camera); err == nil {
		t.Fatal("Run succeeded, want the transient commit failure")
	}
	if got, want := e.staged(t), []string{c.keys.StagingSidecar}; !slices.Equal(got, want) {
		t.Fatalf("staging = %q, want %q kept for the retry", got, want)
	}
	d.failReal = false
	rep := e.mustRun(t, camera)
	wantReport(t, rep, recovery.Report{Recovered: 1})
	if !e.exists(t, c.keys.Sidecar) {
		t.Error("retry did not publish the sidecar")
	}
}
