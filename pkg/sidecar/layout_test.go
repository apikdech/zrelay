package sidecar

import (
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func TestKeysUseSegmentStartDay(t *testing.T) {
	id1 := ulid.MustParse("01M2NBMVS0K8EKRQKGGAJCDT0B")
	id2 := ulid.MustParse("01M2NC3Z8XQ7Y6V5T4S3R2P1N0")

	start1 := time.Date(2026, 9, 16, 23, 59, 59, 999e6, time.UTC)
	k1 := ClipKeys("cam", start1, id1)
	want1 := ClipKeySet{
		Dir:            "clips/cam/2026/09/16",
		Video:          "clips/cam/2026/09/16/235959999_01M2NBMVS0K8EKRQKGGAJCDT0B.zrec",
		Sidecar:        "clips/cam/2026/09/16/235959999_01M2NBMVS0K8EKRQKGGAJCDT0B.json",
		StagingSidecar: "clips/cam/2026/09/16/.235959999_01M2NBMVS0K8EKRQKGGAJCDT0B.json.part",
	}
	if k1 != want1 {
		t.Errorf("ClipKeys(seg 0) = %+v\nwant %+v", k1, want1)
	}

	// The next segment of the same max_duration series starts after
	// midnight and must land in the next day's prefix.
	start2 := time.Date(2026, 9, 17, 0, 4, 59, 0, time.UTC)
	k2 := ClipKeys("cam", start2, id2)
	want2 := ClipKeySet{
		Dir:            "clips/cam/2026/09/17",
		Video:          "clips/cam/2026/09/17/000459000_01M2NC3Z8XQ7Y6V5T4S3R2P1N0.zrec",
		Sidecar:        "clips/cam/2026/09/17/000459000_01M2NC3Z8XQ7Y6V5T4S3R2P1N0.json",
		StagingSidecar: "clips/cam/2026/09/17/.000459000_01M2NC3Z8XQ7Y6V5T4S3R2P1N0.json.part",
	}
	if k2 != want2 {
		t.Errorf("ClipKeys(seg 1) = %+v\nwant %+v", k2, want2)
	}

	// A WIB-local time.Time for the same instants yields the same UTC keys.
	wib := time.FixedZone("WIB", 7*3600)
	if got := ClipKeys("cam", start1.In(wib), id1); got != want1 {
		t.Errorf("WIB seg 0 = %+v, want %+v", got, want1)
	}
	if got := ClipKeys("cam", start2.In(wib), id2); got != want2 {
		t.Errorf("WIB seg 1 = %+v, want %+v", got, want2)
	}

	for _, k := range []ClipKeySet{want1, want2} {
		for _, name := range []string{k.Video[len(k.Dir)+1:], k.Sidecar[len(k.Dir)+1:]} {
			if !NameRE.MatchString(name) {
				t.Errorf("%q does not match NameRE", name)
			}
		}
		if NameRE.MatchString(k.StagingSidecar[len(k.Dir)+1:]) {
			t.Errorf("staging name %q matches NameRE", k.StagingSidecar)
		}
	}
}

func TestLayoutHelpers(t *testing.T) {
	id := ulid.MustParse("01M2NBMVS0K8EKRQKGGAJCDT0B")
	wib := time.FixedZone("WIB", 7*3600)
	start := time.Date(2026, 9, 16, 21, 59, 0, 0, wib) // 14:59Z

	if got, want := ObjectName(start, id, "zrec"), "145900000_01M2NBMVS0K8EKRQKGGAJCDT0B.zrec"; got != want {
		t.Errorf("ObjectName = %q, want %q", got, want)
	}
	if got, want := StagingName("145900000_01M2NBMVS0K8EKRQKGGAJCDT0B.zrec"),
		".145900000_01M2NBMVS0K8EKRQKGGAJCDT0B.zrec.part"; got != want {
		t.Errorf("StagingName = %q, want %q", got, want)
	}
	// 2026-09-17 06:30 WIB is still 2026-09-16 in UTC.
	if got, want := DayPrefix("front-door", time.Date(2026, 9, 17, 6, 30, 0, 0, wib)), "clips/front-door/2026/09/16"; got != want {
		t.Errorf("DayPrefix = %q, want %q", got, want)
	}
	if got, want := CoverageKey("front-door", time.Date(2026, 9, 16, 21, 0, 0, 0, wib)),
		"status/coverage/front-door/2026/09/16/14.json"; got != want {
		t.Errorf("CoverageKey = %q, want %q", got, want)
	}
	if got, want := CoverageKey("front-door", time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)),
		"status/coverage/front-door/2026/01/02/03.json"; got != want {
		t.Errorf("CoverageKey = %q, want %q", got, want)
	}
	if StatusKey != "status/relay.json" {
		t.Errorf("StatusKey = %q", StatusKey)
	}
	if NameRE.String() != `^[0-9]{9}_[0-9A-HJKMNP-TV-Z]{26}\.(zrec|json)$` {
		t.Errorf("NameRE = %s", NameRE)
	}
	for _, bad := range []string{
		"14590000_01M2NBMVS0K8EKRQKGGAJCDT0B.zrec",   // 8 digits
		"145900000_01M2NBMVS0K8EKRQKGGAJCDT0I.zrec",  // I is not Crockford
		"145900000_01M2NBMVS0K8EKRQKGGAJCDT0B.mp4",   // extension
		"a/145900000_01M2NBMVS0K8EKRQKGGAJCDT0B.zrec", // path
	} {
		if NameRE.MatchString(bad) {
			t.Errorf("NameRE matched %q", bad)
		}
	}
}
