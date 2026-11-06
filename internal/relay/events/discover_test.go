package events_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
	"github.com/apikdech/zrelay-v2/internal/testutil"
)

// syncBuffer is a strings.Builder that the test can read while Discover
// writes.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// discover runs events.Discover against f with rules, cancels it once the
// output contains until, and returns the complete output (the suggestions
// follow the listening).
func discover(t *testing.T, f *testutil.FakeONVIF, rules []events.Rule, until string) string {
	t.Helper()
	pp := &events.PullPoint{Camera: "cam", Host: f.Host, Port: f.Port, Username: "admin", Password: "secret", Rules: rules}
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- events.Discover(ctx, pp, time.Hour, &out) }()
	deadline := time.After(20 * time.Second)
	for !strings.Contains(out.String(), until) {
		select {
		case err := <-done:
			t.Fatalf("Discover returned %v before printing %q:\n%s", err, until, out.String())
		case <-deadline:
			t.Fatalf("timed out waiting for %q:\n%s", until, out.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Discover returned %v after cancel, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Discover did not return after cancel")
	}
	return out.String()
}

func TestDiscoverPrintsTopicsAndSuggestsRules(t *testing.T) {
	f := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	f.EventTopic("RuleEngine/CellMotionDetector/Motion", "IsMotion")
	f.EventTopic("RuleEngine/TPSmartEventDetector/TPSmartEvent", "IsPeople")
	f.Queue("tns1:RuleEngine/TPSmartEventDetector/TPSmartEvent", "Initialized", events.Item{Name: "IsPeople", Value: "false"})
	f.Queue("tns1:RuleEngine/TPSmartEventDetector/TPSmartEvent", "Changed", events.Item{Name: "IsPeople", Value: "true"})

	out := discover(t, f, events.TapoRules(), `Name="IsPeople" Value="true"`)

	for _, want := range []string{
		"GetEventProperties",
		"RuleEngine/CellMotionDetector/Motion",
		"IsMotion (xs:boolean)",
		"Initialized",
		"Changed",
		`Name="IsPeople" Value="true"`,
		`Name="VideoSourceConfigurationToken" Value="vsconf"`, // Source items too
		"topic:",
		`item: "IsPeople"`,
		"kind: person",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT COVERED") {
		t.Errorf("TPSmartEvent/IsPeople is covered by the Tapo rules, but output flags it:\n%s", out)
	}
}

func TestDiscoverFlagsTopicsNoRuleCovers(t *testing.T) {
	f := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	f.Queue("tns1:RuleEngine/LineDetector/Crossed", "Changed", events.Item{Name: "ObjectId", Value: "7"})
	f.Queue("tns1:RuleEngine/CellMotionDetector/Motion", "Changed", events.Item{Name: "IsMotion", Value: "true"})

	out := discover(t, f, events.TapoRules(), `Name="IsMotion" Value="true"`)
	if !strings.Contains(out, "NOT COVERED") || !strings.Contains(out, "RuleEngine/LineDetector/Crossed") {
		t.Errorf("uncovered topic not flagged:\n%s", out)
	}
	// Both observed topics get a suggestion, the unknown one as "other".
	if !strings.Contains(out, "kind: motion") || !strings.Contains(out, "kind: other") {
		t.Errorf("suggestions lack motion and other:\n%s", out)
	}
	if !strings.Contains(out, `item: "ObjectId"`) {
		t.Errorf("suggestion lacks the observed item:\n%s", out)
	}
}

func TestDiscoverSurvivesMissingEventProperties(t *testing.T) {
	// The fake answers GetEventProperties with an empty topic set unless
	// topics are registered; a fault must not abort the listening either.
	f := testutil.StartFakeONVIF(t, testutil.FakeONVIFOpts{})
	f.Fault("GetEventProperties")
	f.Queue("tns1:RuleEngine/CellMotionDetector/Motion", "Changed", events.Item{Name: "IsMotion", Value: "true"})

	out := discover(t, f, events.TapoRules(), `Name="IsMotion" Value="true"`)
	if !strings.Contains(out, "GetEventProperties failed") {
		t.Errorf("failure not reported:\n%s", out)
	}
	if !strings.Contains(out, "kind: motion") {
		t.Errorf("listening did not continue:\n%s", out)
	}
}
