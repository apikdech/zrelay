package camera_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

// TestLiveCameraSession is a manual check against a real camera (see
// docs/LIVE-CHECKS.md, Task 6). It is skipped unless ZRELAY_LIVE_RTSP_URL is
// set. The URL holds credentials, so it is never logged.
func TestLiveCameraSession(t *testing.T) {
	rawURL := os.Getenv("ZRELAY_LIVE_RTSP_URL")
	if rawURL == "" {
		t.Skip("ZRELAY_LIVE_RTSP_URL not set")
	}
	seconds := 30
	if s := os.Getenv("ZRELAY_LIVE_RTSP_SECONDS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			t.Fatalf("ZRELAY_LIVE_RTSP_SECONDS = %q, want a positive integer", s)
		}
		seconds = n
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	defer cancel()

	sink := newRecSink()
	err := camera.RunRTSP(ctx, rawURL, sink)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("session ended early: %v", err)
	}
	evs := sink.snapshot()
	cs, aus, as := codecs(evs), videos(evs), audios(evs)
	if len(cs) == 0 || len(aus) == 0 {
		t.Fatalf("no video: %d Codec calls, %d AUs", len(cs), len(aus))
	}
	for i, c := range cs {
		t.Logf("Codec %d: kind=%s %dx%d time_source=%s", i, c.codec.Kind, c.codec.Width, c.codec.Height, c.ts)
	}
	checkDTSStrictlyIncreasing(t, aus)
	var lastIDR int64 = -1
	var maxGOP int64
	for _, au := range aus {
		if au.IDR {
			if lastIDR >= 0 && au.DTS-lastIDR > maxGOP {
				maxGOP = au.DTS - lastIDR
			}
			lastIDR = au.DTS
		}
	}
	last := aus[len(aus)-1]
	t.Logf("%d AUs over %v of media, max GOP %v, wall drift vs media %v, %d audio packets",
		len(aus), time.Duration(last.DTS)*time.Second/90000, time.Duration(maxGOP)*time.Second/90000,
		last.Wall.Sub(aus[0].Wall)-time.Duration(last.DTS-aus[0].DTS)*time.Second/90000, len(as))
	if len(as) > 0 {
		t.Logf("audio codec %s", as[0].Codec)
	}
}
