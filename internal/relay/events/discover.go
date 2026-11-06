package events

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/config"
)

const getEventPropertiesBody = `<GetEventProperties xmlns="` + nsEvents + `"/>`

// RulesFor returns the rules of a camera: the configured ones, or the
// built-in Tapo rules (design §4.3) when brand is "tapo" and none are
// configured.
func RulesFor(brand string, rcs []config.RuleConfig) ([]Rule, error) {
	if len(rcs) == 0 && brand == "tapo" {
		return TapoRules(), nil
	}
	return CompileRules(rcs)
}

// Discover implements "zrelay onvif discover" (relay-spec §4.4). It prints
// the topics and data items the camera advertises (GetEventProperties),
// then follows pp's event subscription for listen (or until ctx is done) and
// prints every notification with its PropertyOperation and every SimpleItem,
// then suggests one rule per observed topic and item and flags the topics
// that pp.Rules do not cover. pp is not modified.
//
// A failed GetEventProperties is reported and the listening goes on. Discover
// returns nil when listening ends; it never prints credentials.
func Discover(ctx context.Context, pp *PullPoint, listen time.Duration, w io.Writer) error {
	cp := *pp
	stats := &discoverMetrics{inner: pp.Metrics}
	cp.Metrics = stats
	d := &discovery{w: w, rules: pp.Rules}
	cp.tap = d.observe

	s := newSession(&cp)
	d.clk = s.clk
	s.syncClock(ctx)
	d.printProperties(ctx, s)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := s.clk.NewTimer(listen)
	defer timer.Stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-timer.C():
			cancel()
		case <-runCtx.Done():
		}
	}()

	fmt.Fprintf(w, "\n== Notifications (listening %s) ==\n", listen)
	// Run returns only when runCtx is done: it reconnects internally.
	_ = cp.Run(runCtx, func(Signal) {}, func(up bool) {
		if up {
			d.printf("[%s] subscription active\n", d.stamp())
		} else {
			d.printf("[%s] subscription ended\n", d.stamp())
		}
	})
	cancel()
	wg.Wait()

	d.printSummary(stats, listen)
	return nil
}

// discoverMetrics counts what the listening shows about the camera.
type discoverMetrics struct {
	inner       PullMetrics
	pullRetries int
	resubscribe int
	skew        *float64
}

func (m *discoverMetrics) PullRetry() {
	m.pullRetries++
	if m.inner != nil {
		m.inner.PullRetry()
	}
}

func (m *discoverMetrics) Resubscribe() {
	m.resubscribe++
	if m.inner != nil {
		m.inner.Resubscribe()
	}
}

func (m *discoverMetrics) Unmatched(topic string) {
	if m.inner != nil {
		m.inner.Unmatched(topic)
	}
}

func (m *discoverMetrics) ClockSkew(seconds float64) {
	m.skew = &seconds
	if m.inner != nil {
		m.inner.ClockSkew(seconds)
	}
}

// observation is one (topic, data item) seen in a notification.
type observation struct {
	topic, item string
	values      []string // distinct, in order of first sight
}

// discovery accumulates what Discover sees. observe runs on the goroutine
// that runs the PullPoint, which is also the one that prints.
type discovery struct {
	w     io.Writer
	clk   interface{ Now() time.Time }
	rules []Rule

	seen   []*observation
	noData map[string]bool // topics seen with no data items
}

func (d *discovery) printf(format string, args ...any) { fmt.Fprintf(d.w, format, args...) }

func (d *discovery) stamp() string { return d.clk.Now().UTC().Format("15:04:05Z") }

func (d *discovery) observe(n notification) {
	op := n.Op
	if op == "" {
		op = "(no PropertyOperation)"
	}
	d.printf("[%s] %s %s\n", d.stamp(), op, n.Topic)
	for _, it := range n.Source {
		d.printf("    source SimpleItem Name=%q Value=%q\n", it.Name, it.Value)
	}
	for _, it := range n.Items {
		d.printf("    data   SimpleItem Name=%q Value=%q\n", it.Name, it.Value)
		d.record(n.Topic, it)
	}
	if len(n.Items) == 0 {
		if d.noData == nil {
			d.noData = map[string]bool{}
		}
		d.noData[n.Topic] = true
	}
}

func (d *discovery) record(topic string, it Item) {
	for _, o := range d.seen {
		if o.topic == topic && o.item == it.Name {
			if !contains(o.values, it.Value) {
				o.values = append(o.values, it.Value)
			}
			return
		}
	}
	d.seen = append(d.seen, &observation{topic: topic, item: it.Name, values: []string{it.Value}})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// topicProps is one advertised topic and its data items.
type topicProps struct {
	topic string
	items []itemDesc
}

type itemDesc struct{ name, typ string }

func (d *discovery) printProperties(ctx context.Context, s *session) {
	d.printf("== GetEventProperties ==\n")
	raw, err := soapCall(ctx, s.client, s.serviceURL(), getEventPropertiesBody, s.token())
	var topics []topicProps
	if err == nil {
		topics, err = parseEventProperties(raw)
	}
	if err != nil {
		d.printf("GetEventProperties failed: %v\n", err)
		return
	}
	if len(topics) == 0 {
		d.printf("(the camera advertises no topics)\n")
	}
	for _, tp := range topics {
		d.printf("topic: %s\n", tp.topic)
		for _, it := range tp.items {
			d.printf("  data item: %s (%s)\n", it.name, it.typ)
		}
	}
}

// parseEventProperties returns the topics of a GetEventPropertiesResponse:
// the elements of the TopicSet marked topic="true", with the
// SimpleItemDescription names and types under their Data element.
func parseEventProperties(raw []byte) ([]topicProps, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var (
		stack    []string
		setIdx   = -1 // index in stack of the TopicSet element
		topicIdx = -1 // index in stack of the current topic element
		inData   bool
		out      []topicProps
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			switch {
			case setIdx < 0:
				if t.Name.Local == "TopicSet" {
					setIdx = len(stack) - 1
				}
			case topicIdx < 0:
				for _, a := range t.Attr {
					if a.Name.Local == "topic" && a.Value == "true" {
						topicIdx = len(stack) - 1
						out = append(out, topicProps{topic: strings.Join(stack[setIdx+1:], "/")})
					}
				}
			default:
				switch t.Name.Local {
				case "Data":
					inData = true
				case "Source":
					inData = false
				case "SimpleItemDescription":
					if inData {
						var it itemDesc
						for _, a := range t.Attr {
							switch a.Name.Local {
							case "Name":
								it.name = a.Value
							case "Type":
								it.typ = a.Value
							}
						}
						out[len(out)-1].items = append(out[len(out)-1].items, it)
					}
				}
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
			if topicIdx >= len(stack) {
				topicIdx, inData = -1, false
			}
			if setIdx >= len(stack) {
				setIdx = -1
			}
		}
	}
}

func (d *discovery) printSummary(stats *discoverMetrics, listen time.Duration) {
	d.printf("\n== Summary ==\n")
	d.printf("pull retries (PullMessages transport errors, normal on Tapo): %d\n", stats.pullRetries)
	d.printf("resubscribes (a failed Renew or pull; 0 over more than 60 s means Renew works): %d\n", stats.resubscribe)
	if stats.skew != nil {
		d.printf("camera clock minus host clock: %.1f s\n", *stats.skew)
	}

	d.printf("\n== Coverage by the configured rules ==\n")
	uncovered := 0
	for _, o := range d.seen {
		if len(Match(d.rules, o.topic, []Item{{Name: o.item, Value: o.values[0]}})) == 0 {
			uncovered++
			d.printf("NOT COVERED: topic %s item %q (no rule matches; it would only count in zrelay_events_unmatched_total)\n", o.topic, o.item)
		}
	}
	for topic := range d.noData {
		d.printf("note: topic %s carried no data items, so no rule can match it\n", topic)
	}
	switch {
	case len(d.seen) == 0:
		d.printf("no notifications were observed: trigger the camera (walk in front of it) and run again\n")
	case uncovered == 0:
		d.printf("every observed topic and item is covered by the configured rules\n")
	}

	d.printf("\n== Suggested rules ==\n")
	if len(d.seen) == 0 {
		d.printf("(none: nothing observed)\n")
		return
	}
	d.printf("rules:\n")
	for _, o := range d.seen {
		d.printf("  - kind: %s\n", suggestKind(o.topic, o.item))
		d.printf("    topic: %s\n", yamlSingleQuote(topicPattern(o.topic)))
		d.printf("    item: %q\n", o.item)
		if !boolValues(o.values) {
			d.printf("    # observed values %q: set active_value to the one that means active\n", o.values)
		}
	}
}

// boolValues reports whether every value is "true" or "false", for which the
// default active_value ("true") is right.
func boolValues(values []string) bool {
	for _, v := range values {
		if v != "true" && v != "false" {
			return false
		}
	}
	return true
}

// suggestKind guesses the trigger kind from the item name, then from the
// last topic segment.
func suggestKind(topic, item string) Kind {
	if k := kindFromName(item); k != KindOther {
		return k
	}
	if i := strings.LastIndexByte(topic, '/'); i >= 0 {
		topic = topic[i+1:]
	}
	return kindFromName(topic)
}

func kindFromName(name string) Kind {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "motion"):
		return KindMotion
	case strings.Contains(n, "people"), strings.Contains(n, "person"), strings.Contains(n, "human"):
		return KindPerson
	case strings.Contains(n, "vehicle"):
		return KindVehicle
	case strings.Contains(n, "pet"), strings.Contains(n, "animal"):
		return KindPet
	}
	return KindOther
}

// topicPattern returns a topic regexp for an observed topic: the topic
// without its namespace prefix, anchored on whole segments like the built-in
// Tapo rules.
func topicPattern(topic string) string {
	if i := strings.IndexByte(topic, ':'); i >= 0 && !strings.Contains(topic[:i], "/") {
		topic = topic[i+1:]
	}
	return `(^|[:/])` + regexp.QuoteMeta(topic) + `$`
}

func yamlSingleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
