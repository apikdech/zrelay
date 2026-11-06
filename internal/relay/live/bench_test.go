package live

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/apikdech/zrelay-v2/internal/relay/camera"
)

const (
	benchViewers = 4
	benchFPS     = 15
	benchBytesPS = 400 << 10 // 400 KiB/s, a 3.2 Mbps Tapo main stream
	benchGOP     = 2 * benchFPS
)

// Env vars that turn TestHelperWHEPClient into a client child process.
const (
	envClientURL   = "ZRELAY_BENCH_WHEP_URL"
	envClientToken = "ZRELAY_BENCH_WHEP_TOKEN"
)

// TestHelperWHEPClient is not a test: BenchmarkViewerMemory re-executes the
// test binary to run it as a viewer in its own process, so the client's
// memory is not counted against the server's. It prints "ready" once video
// flows and exits when stdin closes.
func TestHelperWHEPClient(t *testing.T) {
	base := os.Getenv(envClientURL)
	if base == "" {
		t.Skip("helper process for BenchmarkViewerMemory")
	}
	c, offer := newOfferClient(t, false)
	req, _ := http.NewRequest(http.MethodPost, base+"/live/bench", strings.NewReader(offer))
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Authorization", "Bearer "+os.Getenv(envClientToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status %d", resp.StatusCode)
	}
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}); err != nil {
		t.Fatal(err)
	}
	stdinClosed := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(stdinClosed) }()
	announced := false
	for {
		select {
		case <-c.video:
			if !announced {
				announced = true
				os.Stdout.WriteString("ready\n")
			}
		case <-stdinClosed:
			return
		case <-time.After(60 * time.Second):
			t.Fatal("helper client timed out")
		}
	}
}

// benchAU returns synthetic access unit i of a stream averaging
// benchBytesPS: an IDR five times the size of a P frame.
func benchAU(i int, dts int64) *camera.AccessUnit {
	perGOP := benchBytesPS * benchGOP / benchFPS
	pSize := perGOP / (benchGOP + 4)
	size, hdr, idr := pSize, byte(0x41), false
	if i%benchGOP == 0 {
		size, hdr, idr = 5*pSize, 0x65, true
	}
	nal := bytes.Repeat([]byte{0xaa}, size)
	nal[0] = hdr
	return &camera.AccessUnit{PTS: dts, DTS: dts, IDR: idr, NALUs: [][]byte{nal}, Bytes: size, Wall: time.Now().UTC()}
}

func heapNow() (heap, sys uint64) {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc + m.StackInuse, m.Sys
}

// BenchmarkViewerMemory reports the server-side memory each WHEP viewer
// adds while four clients (separate processes) watch a 400 KiB/s stream.
// Run: go test -run '^$' -bench ViewerMemory -benchtime 1x ./internal/relay/live/
func BenchmarkViewerMemory(b *testing.B) {
	b.StopTimer()
	hub := camera.NewHub("bench")
	hub.Codec(camera.Codec{Kind: "h264", SPS: []byte{0x67, 0x64, 0x00, 0x1f}, PPS: []byte{0x68, 0xee, 0x3c, 0x80}, Width: 640, Height: 360}, camera.TimeSourceArrival)
	ts := newTestServer(b, map[string]*camera.Hub{"bench": hub}, benchViewers)

	pubDone := make(chan struct{})
	stopPub := make(chan struct{})
	go func() {
		defer close(pubDone)
		tick := time.NewTicker(time.Second / benchFPS)
		defer tick.Stop()
		var dts int64
		for i := 0; ; i++ {
			select {
			case <-stopPub:
				return
			case <-tick.C:
			}
			hub.Video(benchAU(i, dts))
			dts += 90000 / benchFPS
		}
	}()
	defer func() { close(stopPub); <-pubDone }()

	var stdins []io.WriteCloser
	var cmds []*exec.Cmd
	defer func() {
		for _, in := range stdins {
			in.Close()
		}
		for _, c := range cmds {
			_ = c.Wait()
		}
	}()

	for n := 0; n < b.N; n++ {
		time.Sleep(time.Second)
		before, sysBefore := heapNow()
		var afterFirst uint64
		for i := 0; i < benchViewers; i++ {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWHEPClient$", "-test.count=1")
			cmd.Env = append(os.Environ(), envClientURL+"="+ts.http.URL, envClientToken+"="+ts.token)
			in, _ := cmd.StdinPipe()
			out, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				b.Fatal(err)
			}
			stdins, cmds = append(stdins, in), append(cmds, cmd)
			ready := make(chan bool, 1)
			go func() {
				sc := bufio.NewScanner(out)
				for sc.Scan() {
					if sc.Text() == "ready" {
						ready <- true
						break
					}
				}
				io.Copy(io.Discard, out)
			}()
			select {
			case <-ready:
			case <-time.After(30 * time.Second):
				b.Fatal("client never received video")
			}
			if i == 0 {
				// Steady state for one viewer, past several GOPs: the
				// difference to four viewers excludes one-time setup.
				time.Sleep(4 * time.Second)
				afterFirst, _ = heapNow()
			}
		}
		if got := ts.Viewers("bench"); got != benchViewers {
			b.Fatalf("Viewers = %d", got)
		}
		// Let the streams reach steady state, past several GOPs.
		time.Sleep(8 * time.Second)
		after, sysAfter := heapNow()
		perViewer := (float64(after) - float64(before)) / benchViewers
		b.ReportMetric(perViewer, "heap-B/viewer")
		b.ReportMetric((float64(sysAfter)-float64(sysBefore))/benchViewers, "sys-B/viewer")
		b.ReportMetric((float64(after)-float64(afterFirst))/(benchViewers-1), "marginal-heap-B/viewer")
	}
}
