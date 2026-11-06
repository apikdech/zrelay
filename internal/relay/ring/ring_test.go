package ring_test

import (
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
	"github.com/apikdech/zrelay-v2/internal/relay/ring"
)

var t0 = time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC)

// au builds an access unit at offset d from t0, with DTS derived from d.
func au(d time.Duration, idr bool, size int) *camera.AccessUnit {
	return &camera.AccessUnit{
		PTS:      int64(d / (time.Second / 90000)),
		DTS:      int64(d / (time.Second / 90000)),
		IDR:      idr,
		NALUs:    [][]byte{make([]byte, size)},
		ParamGen: 1,
		Wall:     t0.Add(d),
		Bytes:    size,
	}
}

func sumBytes(gops []*ring.GOP) int64 {
	var n int64
	for _, g := range gops {
		n += g.Bytes
	}
	return n
}

func TestRetentionKeepsNewestGOPStartingBeforePreRoll(t *testing.T) {
	const preRoll = 15 * time.Second
	r := ring.New(preRoll, 1<<40, func() { t.Fatal("hard cap must not trigger") })

	// A GOP every 2 s, a frame every 500 ms, up to and including t=40 s.
	for d := time.Duration(0); d <= 40*time.Second; d += 500 * time.Millisecond {
		r.Push(au(d, d%(2*time.Second) == 0, 1000))
	}

	now := t0.Add(40 * time.Second)
	cut := now.Add(-preRoll)
	got := r.Retained()
	if len(got) < 2 {
		t.Fatalf("retained %d GOPs, want at least 2", len(got))
	}
	if got[0].Start.After(cut) {
		t.Errorf("first retained GOP starts at %v, want <= %v", got[0].Start, cut)
	}
	if !got[1].Start.After(cut) {
		t.Errorf("second retained GOP starts at %v, want > %v", got[1].Start, cut)
	}
	if want := t0.Add(24 * time.Second); !got[0].Start.Equal(want) {
		t.Errorf("first retained GOP starts at %v, want %v", got[0].Start, want)
	}
	if last := got[len(got)-1]; !last.Start.Equal(now) {
		t.Errorf("last retained GOP starts at %v, want the open GOP at %v", last.Start, now)
	}
	// 24, 26, ..., 40 s.
	if len(got) != 9 {
		t.Errorf("retained %d GOPs, want 9", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i].Start.After(got[i-1].Start) {
			t.Errorf("GOP %d starts at %v, not after GOP %d at %v", i, got[i].Start, i-1, got[i-1].Start)
		}
	}
	if r.Bytes() != sumBytes(got) {
		t.Errorf("Bytes() = %d, want sum of retained %d", r.Bytes(), sumBytes(got))
	}
}

func TestHardCapEvictsAndCounts(t *testing.T) {
	const (
		auSize   = 100
		perGOP   = 4
		capBytes = 1000
		gops     = 6
	)
	truncated := 0
	r := ring.New(time.Hour, capBytes, func() { truncated++ })

	var d time.Duration
	for g := 0; g < gops; g++ {
		for i := 0; i < perGOP; i++ {
			r.Push(au(d, i == 0, auSize))
			d += 250 * time.Millisecond
			if r.Bytes() > capBytes {
				t.Fatalf("after push at %v: Bytes() = %d exceeds cap %d", d, r.Bytes(), capBytes)
			}
		}
	}

	got := r.Retained()
	if truncated == 0 {
		t.Fatal("hard cap never counted a truncation")
	}
	if truncated+len(got) != gops {
		t.Errorf("truncated %d + retained %d != pushed %d GOPs", truncated, len(got), gops)
	}
	// The newest GOPs survive, oldest evicted first, and the open GOP is last.
	wantFirst := t0.Add(time.Duration(gops-len(got)) * time.Second)
	if !got[0].Start.Equal(wantFirst) {
		t.Errorf("first retained GOP starts at %v, want %v", got[0].Start, wantFirst)
	}
	if last := got[len(got)-1]; !last.Start.Equal(t0.Add((gops-1)*time.Second)) || len(last.AUs) != perGOP {
		t.Errorf("last retained GOP = start %v with %d AUs, want the newest GOP with %d AUs", last.Start, len(last.AUs), perGOP)
	}
	if r.Bytes() != sumBytes(got) {
		t.Errorf("Bytes() = %d, want sum of retained %d", r.Bytes(), sumBytes(got))
	}
}

func TestHardCapEvictsOversizedOpenGOP(t *testing.T) {
	truncated := 0
	r := ring.New(time.Hour, 1000, func() { truncated++ })

	r.Push(au(0, true, 600))
	r.Push(au(time.Second, false, 600)) // the only GOP now exceeds the cap
	if truncated != 1 {
		t.Fatalf("truncated = %d, want 1", truncated)
	}
	if r.Bytes() != 0 || len(r.Retained()) != 0 {
		t.Fatalf("ring holds %d bytes in %d GOPs, want empty", r.Bytes(), len(r.Retained()))
	}
	// The evicted GOP's remaining frames have no IDR and are dropped.
	r.Push(au(2*time.Second, false, 10))
	if len(r.Retained()) != 0 {
		t.Fatal("non-IDR after evicting the open GOP was retained")
	}
	r.Push(au(3*time.Second, true, 10))
	if got := r.Retained(); len(got) != 1 || !got[0].Start.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("after the next IDR, retained = %v, want one GOP at t=3s", got)
	}
}

func TestRetainedSharesMemory(t *testing.T) {
	r := ring.New(time.Minute, 1<<30, nil)
	var pushed []*camera.AccessUnit
	for i := 0; i < 10; i++ {
		a := au(time.Duration(i)*100*time.Millisecond, i%5 == 0, 50)
		pushed = append(pushed, a)
		r.Push(a)
	}

	var got []*camera.AccessUnit
	for _, g := range r.Retained() {
		got = append(got, g.AUs...)
	}
	if len(got) != len(pushed) {
		t.Fatalf("retained %d AUs, want %d", len(got), len(pushed))
	}
	for i := range pushed {
		if got[i] != pushed[i] {
			t.Errorf("AU %d: retained pointer %p != pushed %p", i, got[i], pushed[i])
		}
		if &got[i].NALUs[0][0] != &pushed[i].NALUs[0][0] {
			t.Errorf("AU %d: NAL unit bytes were copied", i)
		}
	}
}

func TestResetClears(t *testing.T) {
	r := ring.New(time.Minute, 1<<30, nil)
	r.Push(au(0, true, 100))
	r.Push(au(time.Second, false, 100))
	r.Push(au(2*time.Second, true, 100))

	r.Reset()
	if r.Bytes() != 0 {
		t.Errorf("Bytes() after Reset = %d, want 0", r.Bytes())
	}
	if n := len(r.Retained()); n != 0 {
		t.Errorf("Retained() after Reset has %d GOPs, want 0", n)
	}

	// After Reset the ring waits for the next IDR again.
	r.Push(au(3*time.Second, false, 100))
	if n := len(r.Retained()); n != 0 {
		t.Errorf("non-IDR after Reset retained (%d GOPs)", n)
	}
	r.Push(au(4*time.Second, true, 100))
	if got := r.Retained(); len(got) != 1 || len(got[0].AUs) != 1 {
		t.Errorf("after IDR following Reset, retained %v, want one GOP with one AU", got)
	}
}

func TestNonIDRBeforeFirstIDRDropped(t *testing.T) {
	r := ring.New(time.Minute, 1<<30, nil)
	r.Push(au(0, false, 100))
	r.Push(au(100*time.Millisecond, false, 100))
	if r.Bytes() != 0 || len(r.Retained()) != 0 {
		t.Fatalf("leading non-IDR frames retained: %d bytes", r.Bytes())
	}
	r.Push(au(200*time.Millisecond, true, 100))
	if got := r.Retained(); len(got) != 1 || len(got[0].AUs) != 1 || r.Bytes() != 100 {
		t.Fatalf("after first IDR: %d GOPs, %d bytes; want 1 GOP of 100 bytes", len(got), r.Bytes())
	}
}

func TestOpenGOPIncluded(t *testing.T) {
	r := ring.New(time.Minute, 1<<30, nil)
	idr := au(time.Second, true, 300)
	idr.ParamGen = 7
	r.Push(idr)
	p1 := au(1100*time.Millisecond, false, 20)
	p1.ParamGen = 7
	r.Push(p1)
	p2 := au(1200*time.Millisecond, false, 30)
	p2.ParamGen = 7
	r.Push(p2)

	got := r.Retained()
	if len(got) != 1 {
		t.Fatalf("retained %d GOPs, want the open GOP only", len(got))
	}
	g := got[0]
	if len(g.AUs) != 3 || g.AUs[0] != idr || g.AUs[2] != p2 {
		t.Fatalf("open GOP AUs = %v, want [idr p1 p2]", g.AUs)
	}
	if !g.Start.Equal(idr.Wall) || g.StartDTS != idr.DTS || g.Bytes != 350 || g.ParamGen != 7 {
		t.Errorf("open GOP = {Start %v StartDTS %d Bytes %d ParamGen %d}, want {%v %d 350 7}",
			g.Start, g.StartDTS, g.Bytes, g.ParamGen, idr.Wall, idr.DTS)
	}

	next := au(2*time.Second, true, 400)
	r.Push(next)
	got = r.Retained()
	if len(got) != 2 {
		t.Fatalf("retained %d GOPs after the next IDR, want 2", len(got))
	}
	if len(got[0].AUs) != 3 || got[0].Bytes != 350 {
		t.Errorf("closed GOP has %d AUs / %d bytes, want 3 / 350", len(got[0].AUs), got[0].Bytes)
	}
	if len(got[1].AUs) != 1 || got[1].AUs[0] != next || got[1].Bytes != 400 {
		t.Errorf("new open GOP = %d AUs / %d bytes, want [next] / 400", len(got[1].AUs), got[1].Bytes)
	}
	if r.Bytes() != 750 {
		t.Errorf("Bytes() = %d, want 750", r.Bytes())
	}
}

// A clip may hold the retained GOPs while the camera keeps pushing; the
// snapshot it got must not change under it, and -race must stay quiet.
func TestRetainedStableWhilePushing(t *testing.T) {
	r := ring.New(2*time.Second, 1<<30, nil)
	for i := 0; i < 5; i++ {
		r.Push(au(time.Duration(i)*100*time.Millisecond, i == 0, 10))
	}
	snap := r.Retained()
	n := len(snap[0].AUs)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 5; i < 200; i++ {
			r.Push(au(time.Duration(i)*100*time.Millisecond, i%10 == 0, 10))
		}
	}()
	for i := 0; i < 50; i++ {
		for _, g := range r.Retained() {
			for _, a := range g.AUs {
				_ = a.Bytes
			}
			_ = g.Bytes
		}
	}
	wg.Wait()

	if len(snap) != 1 || len(snap[0].AUs) != n || snap[0].Bytes != int64(n*10) {
		t.Errorf("snapshot changed: %d GOPs, %d AUs, %d bytes; want 1, %d, %d",
			len(snap), len(snap[0].AUs), snap[0].Bytes, n, n*10)
	}
}
