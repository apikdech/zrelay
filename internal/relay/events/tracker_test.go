package events_test

import (
	"slices"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
	"github.com/apikdech/zrelay-v2/internal/relay/events"
)

var t0 = time.Date(2026, 11, 6, 14, 59, 0, 0, time.UTC)

func signal(k events.Kind, active, initialized bool, at time.Time) events.Signal {
	return events.Signal{Camera: "front", Kind: k, Active: active, Initialized: initialized, At: at, RawTopic: motionTopic}
}

// changed drains Changes and reports whether a change was signalled.
func changed(tr *events.Tracker) bool {
	select {
	case <-tr.Changes():
		return true
	default:
		return false
	}
}

func TestTrackerActivates(t *testing.T) {
	tr := events.NewTracker(60*time.Second, clock.NewFake(t0))
	tr.Apply(signal(events.KindPerson, true, false, t0))
	tr.Apply(signal(events.KindMotion, true, false, t0))
	if !tr.Triggered() || !changed(tr) {
		t.Fatal("active signal did not trigger and signal a change")
	}
	if got, want := tr.Active(), []events.Kind{events.KindMotion, events.KindPerson}; !slices.Equal(got, want) {
		t.Fatalf("Active() = %v, want %v", got, want)
	}
	tr.Apply(signal(events.KindMotion, false, false, t0))
	tr.Apply(signal(events.KindPerson, false, false, t0))
	if tr.Triggered() || !changed(tr) {
		t.Fatal("inactive signals did not clear the trigger and signal a change")
	}
}

func TestTrackerTimeoutForcesInactive(t *testing.T) {
	clk := clock.NewFake(t0)
	tr := events.NewTracker(60*time.Second, clk)
	tr.Apply(signal(events.KindMotion, true, false, t0))
	changed(tr)

	clk.Advance(59 * time.Second)
	tr.Expire()
	if !tr.Triggered() {
		t.Fatal("expired before the timeout")
	}
	clk.Advance(time.Second)
	tr.Expire()
	if tr.Triggered() {
		t.Fatal("still triggered 60 s after the last active signal")
	}
	if !changed(tr) {
		t.Fatal("timeout did not signal a change")
	}
}

func TestTrackerActiveRefreshesTimeout(t *testing.T) {
	clk := clock.NewFake(t0)
	tr := events.NewTracker(60*time.Second, clk)
	tr.Apply(signal(events.KindMotion, true, false, t0))
	clk.Advance(40 * time.Second)
	tr.Apply(signal(events.KindMotion, true, false, clk.Now()))
	clk.Advance(40 * time.Second)
	tr.Expire()
	if !tr.Triggered() {
		t.Fatal("a repeated active signal did not refresh the timeout")
	}
}

func TestInitializedActiveSetsStateWithoutNewChange(t *testing.T) {
	tr := events.NewTracker(60*time.Second, clock.NewFake(t0))
	tr.Apply(signal(events.KindMotion, true, false, t0))
	if !changed(tr) {
		t.Fatal("first active signal did not signal a change")
	}
	tr.Apply(signal(events.KindMotion, true, true, t0))
	if changed(tr) {
		t.Fatal("Initialized active on an already-active kind signalled a change")
	}
	if !tr.Triggered() {
		t.Fatal("not triggered")
	}

	// On an inactive kind it sets state as if Changed.
	tr.Apply(signal(events.KindPet, true, true, t0))
	if got, want := tr.Active(), []events.Kind{events.KindMotion, events.KindPet}; !slices.Equal(got, want) {
		t.Fatalf("Active() = %v, want %v", got, want)
	}
}

func TestInitializedInactiveIgnored(t *testing.T) {
	tr := events.NewTracker(60*time.Second, clock.NewFake(t0))
	tr.Apply(signal(events.KindMotion, true, false, t0))
	changed(tr)
	tr.Apply(signal(events.KindMotion, false, true, t0))
	if !tr.Triggered() || changed(tr) {
		t.Fatal("Initialized inactive changed the state")
	}
}

func TestResetAll(t *testing.T) {
	tr := events.NewTracker(60*time.Second, clock.NewFake(t0))
	tr.Apply(signal(events.KindMotion, true, false, t0))
	tr.Apply(signal(events.KindVehicle, true, false, t0))
	changed(tr)
	tr.ResetAll()
	if tr.Triggered() || len(tr.Active()) != 0 {
		t.Fatalf("after ResetAll: triggered=%v active=%v", tr.Triggered(), tr.Active())
	}
	if !changed(tr) {
		t.Fatal("ResetAll did not signal a change")
	}
	tr.ResetAll()
	if changed(tr) {
		t.Fatal("ResetAll with nothing active signalled a change")
	}
}
