package egress

import (
	"bytes"
	"testing"
)

const mib = 1 << 20

// writeAll feeds payload to the clip in 256 KiB writes and finalizes it.
func (c *testClip) writeAll(t *testing.T, payload []byte) {
	t.Helper()
	for off := 0; off < len(payload); off += 256 << 10 {
		c.write(t, payload[off:min(off+256<<10, len(payload))])
	}
	c.finish()
}

func (r *s3Rig) advanceUntil(what string, cond func() bool) {
	r.t.Helper()
	advanceUntil(r.t, r.clk, what, cond)
}

func (r *s3Rig) stagingKey(c *testClip) string { return "cctv/" + c.pc.Keys.StagingSidecar }
func (r *s3Rig) finalKey(c *testClip) string   { return "cctv/" + c.pc.Keys.Sidecar }

func (s *stubS3) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

// A multipart upload removed under a live clip (mc rm --incomplete, stale
// upload expiry) answers NoSuchUpload on UploadPart. That can never succeed:
// once part of the clip was released the clip is abandoned at once (aborted,
// staging sidecar removed, backlog freed) instead of re-uploading forever and
// opening the breaker.
func TestS3PartUploadGoneAbandonsClip(t *testing.T) {
	stub := newStubS3(t)
	stub.partGone = func(id string, part int) bool { return part >= 2 }
	r := newS3Rig(t, stub)
	c := r.begin("A")
	c.writeAll(t, randBytes(11*mib, 3)) // two full parts + a tail

	staging := r.stagingKey(c)
	r.advanceUntil("clip abandoned", func() bool {
		return stub.count("DELETE object") >= 1 && !stub.has(staging) && r.e.BacklogBytes() == 0
	})
	if n := stub.count("PUT part"); n != 2 {
		t.Fatalf("UploadPart calls = %d, want 2 (the vanished upload is not retried)", n)
	}
	if s, _ := r.br.State(); s != Closed {
		t.Fatalf("breaker state = %v, want Closed: a vanished upload is not a storage failure", s)
	}
	if stub.has("cctv/"+c.pc.Keys.Video) || stub.has(r.finalKey(c)) {
		t.Fatal("abandoned clip left a video or a final sidecar")
	}
	if ds := r.dropList(); len(ds) != 0 {
		t.Fatalf("drops = %+v, want none (no coverage reason fits)", ds)
	}
}

// Nothing was released yet: the clip is re-created and replayed from the
// stream, and the stored object equals the stream.
func TestS3PartUploadGoneRestartsFromStream(t *testing.T) {
	stub := newStubS3(t)
	stub.partGone = func(id string, part int) bool { return id == "upload-1" }
	r := newS3Rig(t, stub)
	c := r.begin("A")
	payload := randBytes(6*mib, 5) // one full part: the first UploadPart is the one that fails
	c.writeAll(t, payload)

	r.advanceUntil("clip committed after restart", func() bool { return stub.has(r.finalKey(c)) })
	got, ok := stub.object(c.pc.Keys.Video)
	if !ok || !bytes.Equal(got, payload) {
		t.Fatalf("object differs from the stream (%d vs %d bytes, present=%v)", len(got), len(payload), ok)
	}
	if n := stub.count("POST create"); n != 2 {
		t.Fatalf("multipart uploads created = %d, want 2", n)
	}
	if s, _ := r.br.State(); s != Closed {
		t.Fatalf("breaker state = %v, want Closed", s)
	}
	if ds := r.dropList(); len(ds) != 0 {
		t.Fatalf("drops = %+v, want none", ds)
	}
}

// The upload vanishes before CompleteMultipartUpload and the object is not
// there: the driver's Commit error is Permanent, but only a restart from the
// stream (which still holds every byte) can store the clip.
func TestS3CommitUploadGoneRestartsFromStream(t *testing.T) {
	stub := newStubS3(t)
	stub.completeGone = func(id string) bool { return id == "upload-1" }
	r := newS3Rig(t, stub)
	c := r.begin("A")
	payload := randBytes(1*mib, 9)
	c.writeAll(t, payload)

	r.advanceUntil("clip committed after restart", func() bool { return stub.has(r.finalKey(c)) })
	got, ok := stub.object(c.pc.Keys.Video)
	if !ok || !bytes.Equal(got, payload) {
		t.Fatalf("object differs from the stream (%d vs %d bytes, present=%v)", len(got), len(payload), ok)
	}
	if n := stub.count("POST create"); n != 2 {
		t.Fatalf("multipart uploads created = %d, want 2", n)
	}
	if s, _ := r.br.State(); s != Closed {
		t.Fatalf("breaker state = %v, want Closed", s)
	}
}

// CompleteMultipartUpload succeeded but its response was lost; the clip is
// then evicted while Commit is retrying. Abort finds the object published, so
// the clip is committed: its final sidecar must be stored and the staging
// sidecar removed, not deleted with the video left invisible.
func TestS3EvictionAfterLostCompleteKeepsCommittedClip(t *testing.T) {
	stub := newStubS3(t)
	stub.dropComplete = 1
	r := newS3Rig(t, stub)
	c := r.begin("A")
	payload := randBytes(1*mib, 13)
	c.writeAll(t, payload)

	eventually(t, "CompleteMultipartUpload sent", func() bool { return stub.count("POST complete") == 1 })
	r.e.mu.Lock()
	var clip *Clip
	for k := range r.e.jobs {
		clip = k
	}
	r.e.mu.Unlock()
	clip.Stream.Evict() // the backlog policy evicts the clip being drained

	final, staging := r.finalKey(c), r.stagingKey(c)
	eventually(t, "final sidecar of the committed clip", func() bool { return stub.has(final) })
	eventually(t, "job finished", func() bool {
		r.e.mu.Lock()
		defer r.e.mu.Unlock()
		return len(r.e.jobs) == 0
	})
	if stub.has(staging) {
		t.Fatal("staging sidecar left next to the final one")
	}
	if got, ok := stub.object(c.pc.Keys.Video); !ok || !bytes.Equal(got, payload) {
		t.Fatal("committed video missing or differs")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.done) != 1 {
		t.Fatalf("OnCommitted fired %d times, want 1", len(r.done))
	}
}
