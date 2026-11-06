package testutil

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
)

// FakeONVIFOpts configures StartFakeONVIF.
type FakeONVIFOpts struct {
	// Username and Password are the credentials the WS-UsernameToken digest
	// is checked against. They default to "admin" and "secret".
	Username, Password string
}

// RecordedRequest is one SOAP request the fake camera received.
type RecordedRequest struct {
	// Op is the local name of the first element in the SOAP body, for
	// example "PullMessages".
	Op string
	// Path is the request URI (path and query).
	Path string
	// Host is the Host header.
	Host string
	// LocalPort is the port of the fake's listener that took the request.
	LocalPort int
	// HasToken reports whether the request carried a UsernameToken.
	HasToken bool
	// DigestValid reports whether the UsernameToken digest matches the
	// configured credentials.
	DigestValid bool
	// Created is the UsernameToken Created text.
	Created string
	// Close reports whether the request asked to close the connection.
	Close  bool
	Header http.Header
	Body   string
}

// FakeONVIF is an in-process ONVIF camera serving the device service
// (GetSystemDateAndTime) and a PullPoint event service like a Tapo camera.
// It answers on two listeners, Port and SubPort, with the same handler.
type FakeONVIF struct {
	// Host is the listen address host, "127.0.0.1".
	Host string
	// Port is the main listener port (the configured events port).
	Port int
	// SubPort is a second listener port, for subscription addresses that
	// name another port.
	SubPort int

	opts FakeONVIFOpts

	mu         sync.Mutex
	requests   []RecordedRequest
	queue      []fakeMessage
	drops      int
	faults     map[string]int
	redirects  map[string]string
	subAddr    string
	subSeq     int
	cameraTime time.Time
	pad        int
	topics     []fakeTopic
}

type fakeTopic struct {
	path  string
	items []string
}

type fakeMessage struct {
	topic, op string
	utc       time.Time
	items     []events.Item
}

// StartFakeONVIF starts the fake camera on two ephemeral 127.0.0.1 ports.
// It stops when the test ends.
func StartFakeONVIF(t testing.TB, opts FakeONVIFOpts) *FakeONVIF {
	t.Helper()
	if opts.Username == "" {
		opts.Username = "admin"
	}
	if opts.Password == "" {
		opts.Password = "secret"
	}
	f := &FakeONVIF{Host: "127.0.0.1", opts: opts, faults: map[string]int{}, redirects: map[string]string{}}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 5 * time.Second}
	for _, port := range []*int{&f.Port, &f.SubPort} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("fake onvif: listen: %v", err)
		}
		*port = l.Addr().(*net.TCPAddr).Port
		go func() { _ = srv.Serve(l) }()
	}
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

// Queue adds a notification that the next PullMessages returns. op is the
// PropertyOperation, "Initialized" or "Changed". The message UtcTime is the
// camera time when Queue is called.
func (f *FakeONVIF) Queue(topic string, op string, items ...events.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, fakeMessage{topic: topic, op: op, utc: f.nowLocked(), items: items})
}

// DropNextPulls makes the next n PullMessages requests fail at the
// transport: the connection is closed with no reply, as Tapo firmware does
// when nothing is queued.
func (f *FakeONVIF) DropNextPulls(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drops += n
}

// FailRenew makes the next Renew answer with a SOAP fault.
func (f *FakeONVIF) FailRenew() { f.Fault("Renew") }

// Fault makes the next request for op answer HTTP 500 with a SOAP fault.
// Each call adds one fault.
func (f *FakeONVIF) Fault(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[op]++
}

// Redirect makes every request for op answer 307 Temporary Redirect to
// location.
func (f *FakeONVIF) Redirect(op, location string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redirects[op] = location
}

// SubscriptionAddress sets the address CreatePullPointSubscription returns,
// e.g. "http://127.0.0.1:<port>/sub". By default it is
// "http://127.0.0.1:<Port>/sub?id=<n>".
func (f *FakeONVIF) SubscriptionAddress(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subAddr = addr
}

// CameraTime fixes the camera clock that GetSystemDateAndTime reports and
// that queued messages carry as UtcTime. By default it is the host clock.
func (f *FakeONVIF) CameraTime(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cameraTime = t.UTC()
}

// PadNextPull pads the next PullMessages reply with an XML comment of n
// bytes, so the reply stays valid XML but grows past a size cap.
func (f *FakeONVIF) PadNextPull(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pad = n
}

// EventTopic registers a topic that GetEventProperties lists, with the names
// of its boolean data items. path has no namespace prefix and at least two
// segments, for example "RuleEngine/CellMotionDetector/Motion". By default the topic set is empty.
func (f *FakeONVIF) EventTopic(path string, items ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.topics = append(f.topics, fakeTopic{path: path, items: items})
}

// Requests returns every request received so far, in arrival order.
func (f *FakeONVIF) Requests() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RecordedRequest(nil), f.requests...)
}

// Count returns how many requests for op were received.
func (f *FakeONVIF) Count(op string) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Op == op {
			n++
		}
	}
	return n
}

func (f *FakeONVIF) nowLocked() time.Time {
	if !f.cameraTime.IsZero() {
		return f.cameraTime
	}
	return time.Now().UTC()
}

// soapRequest is the part of a SOAP request the fake inspects.
type soapRequest struct {
	Header struct {
		Token struct {
			Username string `xml:"Username"`
			Password string `xml:"Password"`
			Nonce    string `xml:"Nonce"`
			Created  string `xml:"Created"`
		} `xml:"Security>UsernameToken"`
	} `xml:"Header"`
	Body struct {
		Inner []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"Body"`
}

func (f *FakeONVIF) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var env soapRequest
	_ = xml.Unmarshal(raw, &env)
	op := ""
	if len(env.Body.Inner) > 0 {
		op = env.Body.Inner[0].XMLName.Local
	}
	tok := env.Header.Token
	rec := RecordedRequest{
		Op:       op,
		Path:     r.URL.RequestURI(),
		Host:     r.Host,
		HasToken: tok.Username != "",
		Created:  strings.TrimSpace(tok.Created),
		Close:    r.Close,
		Header:   r.Header.Clone(),
		Body:     string(raw),
	}
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr); ok {
		rec.LocalPort = a.Port
	}
	rec.DigestValid = rec.HasToken && tok.Username == f.opts.Username && validDigest(tok.Nonce, tok.Created, f.opts.Password, tok.Password)

	f.mu.Lock()
	f.requests = append(f.requests, rec)
	if loc, ok := f.redirects[op]; ok {
		f.mu.Unlock()
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	if op == "PullMessages" && f.drops > 0 {
		f.drops--
		f.mu.Unlock()
		hijackClose(w)
		return
	}
	if f.faults[op] > 0 {
		f.faults[op]--
		f.mu.Unlock()
		writeFault(w, http.StatusInternalServerError, "ter:Action", "fault injected")
		return
	}
	camNow := f.nowLocked()
	f.mu.Unlock()

	if op != "GetSystemDateAndTime" && !rec.DigestValid {
		writeFault(w, http.StatusBadRequest, "ter:NotAuthorized", "Sender not authorized")
		return
	}
	switch op {
	case "GetSystemDateAndTime":
		writeEnvelope(w, systemDateAndTime(camNow))
	case "CreatePullPointSubscription":
		f.mu.Lock()
		f.subSeq++
		addr := f.subAddr
		if addr == "" {
			addr = fmt.Sprintf("http://127.0.0.1:%d/sub?id=%d", f.Port, f.subSeq)
		}
		f.mu.Unlock()
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(addr))
		writeEnvelope(w, fmt.Sprintf(`<tev:CreatePullPointSubscriptionResponse><tev:SubscriptionReference><wsa5:Address>%s</wsa5:Address></tev:SubscriptionReference><wsnt:CurrentTime>%s</wsnt:CurrentTime><wsnt:TerminationTime>%s</wsnt:TerminationTime></tev:CreatePullPointSubscriptionResponse>`,
			b.String(), camNow.Format(time.RFC3339), camNow.Add(120*time.Second).Format(time.RFC3339)))
	case "GetEventProperties":
		writeEnvelope(w, f.eventProperties())
	case "PullMessages":
		writeEnvelope(w, f.pullReply(camNow))
	case "Renew":
		writeEnvelope(w, fmt.Sprintf(`<wsnt:RenewResponse><wsnt:TerminationTime>%s</wsnt:TerminationTime><wsnt:CurrentTime>%s</wsnt:CurrentTime></wsnt:RenewResponse>`,
			camNow.Add(120*time.Second).Format(time.RFC3339), camNow.Format(time.RFC3339)))
	case "Unsubscribe":
		writeEnvelope(w, `<wsnt:UnsubscribeResponse/>`)
	default:
		writeFault(w, http.StatusBadRequest, "ter:ActionNotSupported", "unsupported")
	}
}

// eventProperties renders the topic set as a Tapo camera does: the first
// path segment carries the tns1 prefix, the last one wstop:topic="true" and a
// MessageDescription.
func (f *FakeONVIF) eventProperties() string {
	f.mu.Lock()
	topics := append([]fakeTopic(nil), f.topics...)
	f.mu.Unlock()

	var b strings.Builder
	b.WriteString(`<tev:GetEventPropertiesResponse><tev:TopicNamespaceLocation>http://www.onvif.org/onvif/ver10/topics/topicns.xml</tev:TopicNamespaceLocation><wsnt:FixedTopicSet>true</wsnt:FixedTopicSet><wstop:TopicSet xmlns:wstop="http://docs.oasis-open.org/wsn/t-1" xmlns:tns1="http://www.onvif.org/ver10/topics">`)
	for _, tp := range topics {
		segs := strings.Split(tp.path, "/")
		if len(segs) < 2 {
			continue // needs a namespaced root and a topic element
		}
		for i, seg := range segs {
			switch {
			case i == 0:
				fmt.Fprintf(&b, "<tns1:%s>", seg)
			case i == len(segs)-1:
				fmt.Fprintf(&b, `<%s wstop:topic="true"><tt:MessageDescription IsProperty="true"><tt:Source><tt:SimpleItemDescription Name="VideoSourceConfigurationToken" Type="tt:ReferenceToken"/></tt:Source><tt:Data>`, seg)
				for _, it := range tp.items {
					fmt.Fprintf(&b, `<tt:SimpleItemDescription Name="%s" Type="xs:boolean"/>`, escape(it))
				}
				b.WriteString(`</tt:Data></tt:MessageDescription>`)
			default:
				fmt.Fprintf(&b, "<%s>", seg)
			}
		}
		for i := len(segs) - 1; i >= 0; i-- {
			if i == 0 {
				fmt.Fprintf(&b, "</tns1:%s>", segs[i])
			} else {
				fmt.Fprintf(&b, "</%s>", segs[i])
			}
		}
	}
	b.WriteString(`</wstop:TopicSet></tev:GetEventPropertiesResponse>`)
	return b.String()
}

func (f *FakeONVIF) pullReply(camNow time.Time) string {
	f.mu.Lock()
	n := min(len(f.queue), 20)
	msgs := f.queue[:n]
	f.queue = append([]fakeMessage(nil), f.queue[n:]...)
	pad := f.pad
	f.pad = 0
	f.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, `<tev:PullMessagesResponse><tev:CurrentTime>%s</tev:CurrentTime><tev:TerminationTime>%s</tev:TerminationTime>`,
		camNow.Format(time.RFC3339), camNow.Add(120*time.Second).Format(time.RFC3339))
	if pad > 0 {
		b.WriteString("<!--")
		b.WriteString(strings.Repeat("x", pad))
		b.WriteString("-->")
	}
	for _, m := range msgs {
		fmt.Fprintf(&b, `<wsnt:NotificationMessage><wsnt:Topic Dialect="http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet">%s</wsnt:Topic><wsnt:Message><tt:Message UtcTime="%s" PropertyOperation="%s"><tt:Source><tt:SimpleItem Name="VideoSourceConfigurationToken" Value="vsconf"/></tt:Source><tt:Data>`,
			escape(m.topic), m.utc.Format(time.RFC3339), escape(m.op))
		for _, it := range m.items {
			fmt.Fprintf(&b, `<tt:SimpleItem Name="%s" Value="%s"/>`, escape(it.Name), escape(it.Value))
		}
		b.WriteString(`</tt:Data></tt:Message></wsnt:Message></wsnt:NotificationMessage>`)
	}
	b.WriteString(`</tev:PullMessagesResponse>`)
	return b.String()
}

func systemDateAndTime(t time.Time) string {
	return fmt.Sprintf(`<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime><tt:DateTimeType>NTP</tt:DateTimeType><tt:DaylightSavings>false</tt:DaylightSavings><tt:TimeZone><tt:TZ>UTC0</tt:TZ></tt:TimeZone><tt:UTCDateTime><tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time><tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date></tt:UTCDateTime></tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`,
		t.Hour(), t.Minute(), t.Second(), t.Year(), int(t.Month()), t.Day())
}

const fakeEnvelopeOpen = `<?xml version="1.0" encoding="UTF-8"?><SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope" xmlns:wsa5="http://www.w3.org/2005/08/addressing" xmlns:tt="http://www.onvif.org/ver10/schema" xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:tev="http://www.onvif.org/ver10/events/wsdl" xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2" xmlns:ter="http://www.onvif.org/ver10/error"><SOAP-ENV:Body>`

const fakeEnvelopeClose = `</SOAP-ENV:Body></SOAP-ENV:Envelope>`

func writeEnvelope(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	_, _ = io.WriteString(w, fakeEnvelopeOpen+body+fakeEnvelopeClose)
}

func writeFault(w http.ResponseWriter, status int, subcode, reason string) {
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `%s<SOAP-ENV:Fault><SOAP-ENV:Code><SOAP-ENV:Value>SOAP-ENV:Sender</SOAP-ENV:Value><SOAP-ENV:Subcode><SOAP-ENV:Value>%s</SOAP-ENV:Value></SOAP-ENV:Subcode></SOAP-ENV:Code><SOAP-ENV:Reason><SOAP-ENV:Text xml:lang="en">%s</SOAP-ENV:Text></SOAP-ENV:Reason></SOAP-ENV:Fault>%s`,
		fakeEnvelopeOpen, subcode, escape(reason), fakeEnvelopeClose)
}

// hijackClose drops the connection with no reply.
func hijackClose(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(errors.New("fake onvif: response writer cannot hijack"))
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func validDigest(nonceB64, created, password, digestB64 string) bool {
	nonce, err := base64.StdEncoding.DecodeString(strings.TrimSpace(nonceB64))
	if err != nil {
		return false
	}
	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(strings.TrimSpace(created)))
	h.Write([]byte(password))
	return base64.StdEncoding.EncodeToString(h.Sum(nil)) == strings.TrimSpace(digestB64)
}

func escape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
