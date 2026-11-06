package clock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
)

var epoch = time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC)

// fired reports whether a value is ready on c without blocking.
func fired(c <-chan time.Time) (time.Time, bool) {
	select {
	case v := <-c:
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestFakeNowStartsAtStartAndAdvances(t *testing.T) {
	f := clock.NewFake(epoch)
	if got := f.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() = %v, want %v", got, epoch)
	}
	f.Advance(90 * time.Second)
	if got, want := f.Now(), epoch.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("Now() after Advance = %v, want %v", got, want)
	}
}

func TestFakeTimerFiresOnAdvance(t *testing.T) {
	f := clock.NewFake(epoch)
	tm := f.NewTimer(5 * time.Second)

	f.Advance(4 * time.Second)
	if _, ok := fired(tm.C()); ok {
		t.Fatal("timer fired after 4s, want not before 5s")
	}

	f.Advance(1 * time.Second)
	v, ok := fired(tm.C())
	if !ok {
		t.Fatal("timer did not fire after 5s")
	}
	if want := epoch.Add(5 * time.Second); !v.Equal(want) {
		t.Fatalf("timer value = %v, want %v", v, want)
	}
	if tm.Stop() {
		t.Fatal("Stop() on a fired timer = true, want false")
	}
}

func TestFakeAdvanceFiresDueTimersInDeadlineOrder(t *testing.T) {
	f := clock.NewFake(epoch)
	late := f.NewTimer(3 * time.Second)
	early := f.NewTimer(1 * time.Second)
	notDue := f.NewTimer(10 * time.Second)

	f.Advance(5 * time.Second)

	ve, ok := fired(early.C())
	if !ok {
		t.Fatal("1s timer did not fire")
	}
	vl, ok := fired(late.C())
	if !ok {
		t.Fatal("3s timer did not fire")
	}
	if !ve.Equal(epoch.Add(1*time.Second)) || !vl.Equal(epoch.Add(3*time.Second)) {
		t.Fatalf("fire values = %v, %v; want each timer's own deadline", ve, vl)
	}
	if _, ok := fired(notDue.C()); ok {
		t.Fatal("10s timer fired after 5s")
	}
	if got, want := f.Now(), epoch.Add(5*time.Second); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
}

func TestFakeTimerResetAndStop(t *testing.T) {
	f := clock.NewFake(epoch)
	tm := f.NewTimer(5 * time.Second)

	if !tm.Stop() {
		t.Fatal("Stop() on a pending timer = false, want true")
	}
	f.Advance(10 * time.Second)
	if _, ok := fired(tm.C()); ok {
		t.Fatal("stopped timer fired")
	}
	if tm.Stop() {
		t.Fatal("second Stop() = true, want false")
	}

	// Reset re-arms relative to the current fake time.
	tm.Reset(2 * time.Second)
	f.Advance(1 * time.Second)
	if _, ok := fired(tm.C()); ok {
		t.Fatal("reset timer fired early")
	}
	f.Advance(1 * time.Second)
	v, ok := fired(tm.C())
	if !ok {
		t.Fatal("reset timer did not fire")
	}
	if want := epoch.Add(12 * time.Second); !v.Equal(want) {
		t.Fatalf("reset timer value = %v, want %v", v, want)
	}

	// Reset of a pending timer moves its deadline.
	tm.Reset(5 * time.Second)
	tm.Reset(1 * time.Second)
	f.Advance(1 * time.Second)
	if _, ok := fired(tm.C()); !ok {
		t.Fatal("timer reset to a shorter duration did not fire")
	}
}

func TestFakeStopAndResetDiscardStaleValue(t *testing.T) {
	f := clock.NewFake(epoch)
	tm := f.NewTimer(1 * time.Second)
	f.Advance(1 * time.Second) // fires, value left unread

	tm.Reset(3 * time.Second)
	if _, ok := fired(tm.C()); ok {
		t.Fatal("stale value received after Reset")
	}

	f.Advance(3 * time.Second) // fires again, value left unread
	tm.Stop()
	if _, ok := fired(tm.C()); ok {
		t.Fatal("stale value received after Stop")
	}
}

func TestFakeSleepReturnsOnContextCancel(t *testing.T) {
	f := clock.NewFake(epoch)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- f.Sleep(ctx, time.Hour) }()

	f.BlockUntil(1)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Sleep() = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sleep did not return after context cancel")
	}

	// The cancelled sleep must not leave a pending timer behind.
	f.Advance(2 * time.Hour)
	if n := f.Pending(); n != 0 {
		t.Fatalf("Pending() = %d after cancelled Sleep, want 0", n)
	}
}

func TestFakeSleepReturnsAfterAdvance(t *testing.T) {
	f := clock.NewFake(epoch)

	done := make(chan error, 1)
	go func() { done <- f.Sleep(context.Background(), 30*time.Second) }()

	f.BlockUntil(1)
	f.Advance(29 * time.Second)
	select {
	case err := <-done:
		t.Fatalf("Sleep returned early with %v", err)
	default:
	}

	f.Advance(1 * time.Second)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sleep() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sleep did not return after its duration elapsed")
	}
}

func TestFakeSleepNonPositiveDuration(t *testing.T) {
	f := clock.NewFake(epoch)
	if err := f.Sleep(context.Background(), 0); err != nil {
		t.Fatalf("Sleep(0) = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Sleep(ctx, -time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep(cancelled, -1s) = %v, want context.Canceled", err)
	}
}

func TestFakeTimerZeroDurationFiresImmediately(t *testing.T) {
	f := clock.NewFake(epoch)
	tm := f.NewTimer(0)
	v, ok := fired(tm.C())
	if !ok {
		t.Fatal("zero-duration timer did not fire without Advance")
	}
	if !v.Equal(epoch) {
		t.Fatalf("value = %v, want %v", v, epoch)
	}
}

func TestFakeBlockUntilWaitsForTimers(t *testing.T) {
	f := clock.NewFake(epoch)
	released := make(chan struct{})
	go func() {
		f.BlockUntil(2)
		close(released)
	}()

	f.NewTimer(time.Second)
	select {
	case <-released:
		t.Fatal("BlockUntil(2) returned with 1 pending timer")
	case <-time.After(20 * time.Millisecond):
	}

	f.NewTimer(time.Second)
	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("BlockUntil(2) did not return with 2 pending timers")
	}
}

func TestFakeImplementsClock(t *testing.T) {
	var _ clock.Clock = clock.NewFake(epoch)
}

func TestRealNowIsUTCWallTime(t *testing.T) {
	before := time.Now()
	got := clock.Real.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Real.Now() = %v, want between %v and %v", got, before, after)
	}
	if got.Location() != time.UTC {
		t.Fatalf("Real.Now() location = %v, want UTC", got.Location())
	}
}

func TestRealTimerFires(t *testing.T) {
	tm := clock.Real.NewTimer(time.Millisecond)
	select {
	case <-tm.C():
	case <-time.After(10 * time.Second):
		t.Fatal("real timer did not fire")
	}
	if tm.Stop() {
		t.Fatal("Stop() on fired real timer = true, want false")
	}
	tm.Reset(time.Hour)
	if !tm.Stop() {
		t.Fatal("Stop() on reset real timer = false, want true")
	}
}

func TestRealSleepReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clock.Real.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Real.Sleep(cancelled) = %v, want context.Canceled", err)
	}
	if err := clock.Real.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Real.Sleep(1ms) = %v, want nil", err)
	}
}
