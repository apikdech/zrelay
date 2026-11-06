package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/apikdech/zrelay-v2/internal/clock"
)

// PullPoint cadence and limits (design §4.3).
const (
	defaultServicePath = "/onvif/service"
	// deviceServicePath is where endpoint discovery asks (design §4.3).
	deviceServicePath = "/onvif/device_service"
	pullEvery         = time.Second
	renewEvery        = 60 * time.Second
	subscribeFor      = 120 * time.Second
	clockSyncEvery    = 30 * time.Minute
	requestTimeout    = 5 * time.Second
	minRetry          = 5 * time.Second
	maxRetry          = 60 * time.Second
)

var (
	createBody = fmt.Sprintf(`<CreatePullPointSubscription xmlns="%s"><InitialTerminationTime>PT%dS</InitialTerminationTime></CreatePullPointSubscription>`,
		nsEvents, int(subscribeFor.Seconds()))
	pullBody = fmt.Sprintf(`<PullMessages xmlns="%s"><Timeout>PT%dS</Timeout><MessageLimit>20</MessageLimit></PullMessages>`,
		nsEvents, int(pullEvery.Seconds()))
	renewBody = fmt.Sprintf(`<Renew xmlns="%s"><TerminationTime>PT%dS</TerminationTime></Renew>`,
		nsWSN, int(subscribeFor.Seconds()))
	unsubscribeBody = fmt.Sprintf(`<Unsubscribe xmlns="%s"/>`, nsWSN)
	systemTimeBody  = fmt.Sprintf(`<GetSystemDateAndTime xmlns="%s"/>`, nsDevice)

	capabilitiesBody = fmt.Sprintf(`<GetCapabilities xmlns="%s"><Category>Events</Category></GetCapabilities>`, nsDevice)
	servicesBody     = fmt.Sprintf(`<GetServices xmlns="%s"><IncludeCapability>false</IncludeCapability></GetServices>`, nsDevice)
)

// PullMetrics receives the PullPoint client's counters (relay-spec §4.12).
type PullMetrics interface {
	// PullRetry counts a PullMessages transport error, retried on the same
	// subscription.
	PullRetry()
	// Resubscribe counts a subscription attempt after the first.
	Resubscribe()
	// Unmatched counts a notification no rule matched.
	Unmatched(topic string)
	// ClockSkew reports the camera clock minus the host clock.
	ClockSkew(seconds float64)
}

// PullPoint is an ONVIF PullPoint event Source (relay-spec §4.4, design
// §4.3). It is a minimal SOAP client tuned for Tapo firmware.
type PullPoint struct {
	Camera string
	// Host is the camera host. Every request goes to it, whatever host the
	// camera names in its subscription address.
	Host string
	Port int
	// ServicePath is the event service path, "/onvif/service" when empty.
	// Only when it is empty and the default fails is the path discovered
	// with GetCapabilities or GetServices (design §4.3).
	ServicePath        string
	Username, Password string
	Rules              []Rule
	// Clock defaults to clock.Real.
	Clock clock.Clock
	// Metrics may be nil.
	Metrics PullMetrics

	// tap, if set, sees every notification before the rules do (Discover).
	tap func(notification)
}

// Run subscribes, pulls and renews until ctx is done. It calls ready(true)
// once a subscription is up and ready(false) when it ends, after forcing
// every kind inactive. Failed subscriptions are retried with backoff.
func (p *PullPoint) Run(ctx context.Context, emit func(Signal), ready func(bool)) error {
	s := newSession(p)
	delay := minRetry
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			s.metrics.Resubscribe()
		}
		addr, err := s.subscribe(ctx)
		if err == nil {
			ready(true)
			s.healthy = false
			err = s.follow(ctx, addr, emit)
			if s.healthy {
				// It worked for a while: a new failure starts the backoff over.
				delay = minRetry
			}
			s.unsubscribe(addr)
			s.resetKinds(emit)
			ready(false)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("onvif events interrupted", "camera", p.Camera, "err", err, "retry_in", delay)
		if err := s.clk.Sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, maxRetry)
	}
}

// newSession returns the state of a Run of p, with defaults filled in.
func newSession(p *PullPoint) *session {
	s := &session{
		p:       p,
		clk:     p.Clock,
		metrics: p.Metrics,
		client: &http.Client{
			Timeout:   requestTimeout,
			Transport: &http.Transport{DisableKeepAlives: true},
			// Requests carry a login token, which a redirect would send elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if s.clk == nil {
		s.clk = clock.Real
	}
	if s.metrics == nil {
		s.metrics = nopMetrics{}
	}
	return s
}

// session is the state of one Run.
type session struct {
	p       *PullPoint
	clk     clock.Clock
	metrics PullMetrics
	client  *http.Client
	offset  time.Duration // camera clock minus host clock
	// healthy is set once the current subscription has proven itself: a
	// PullMessages reply came back whole or a Renew succeeded. Creating a
	// subscription alone proves nothing (a camera can accept it and reject
	// every pull), so only a healthy one resets the resubscribe backoff.
	healthy bool
	// eventPath and devicePath are the paths found by endpoint discovery,
	// kept for the session. Empty means the configured or default path.
	eventPath, devicePath string
}

// servicePath is the event service path in use.
func (s *session) servicePath() string {
	switch {
	case s.eventPath != "":
		return s.eventPath
	case s.p.ServicePath != "":
		return s.p.ServicePath
	}
	return defaultServicePath
}

// serviceURL is the event service endpoint.
func (s *session) serviceURL() string { return s.onHost(s.servicePath()) }

// deviceURL is the endpoint of GetSystemDateAndTime: the device service once
// discovery has found it, else the event service endpoint, as on Tapo.
func (s *session) deviceURL() string {
	if s.devicePath != "" {
		return s.onHost(s.devicePath)
	}
	return s.serviceURL()
}

// onHost is path on the configured host and port.
func (s *session) onHost(path string) string {
	return "http://" + net.JoinHostPort(s.p.Host, strconv.Itoa(s.p.Port)) + path
}

func (s *session) token() *token {
	return &token{username: s.p.Username, password: s.p.Password, created: s.clk.Now().Add(s.offset)}
}

// syncClock refreshes the camera clock offset. On failure the previous
// offset stays.
func (s *session) syncClock(ctx context.Context) {
	raw, err := soapCall(ctx, s.client, s.deviceURL(), systemTimeBody, nil)
	var cam time.Time
	if err == nil {
		cam, err = parseSystemTime(raw)
	}
	if err != nil {
		slog.Warn("onvif GetSystemDateAndTime failed", "camera", s.p.Camera, "err", err)
		return
	}
	s.offset = cam.Sub(s.clk.Now())
	s.metrics.ClockSkew(s.offset.Seconds())
}

func (s *session) subscribe(ctx context.Context) (string, error) {
	s.syncClock(ctx)
	raw, err := soapCall(ctx, s.client, s.serviceURL(), createBody, s.token())
	if s.shouldDiscover(err) && s.discover(ctx) {
		s.syncClock(ctx)
		raw, err = soapCall(ctx, s.client, s.serviceURL(), createBody, s.token())
	}
	if err != nil {
		return "", fmt.Errorf("CreatePullPointSubscription: %w", err)
	}
	addr, err := parseSubscription(raw)
	if err != nil {
		return "", fmt.Errorf("CreatePullPointSubscription: %w", err)
	}
	return s.onCamera(addr)
}

// shouldDiscover reports whether err, from the event service, calls for
// endpoint discovery: no path is configured, none was discovered yet, and the
// camera answered with a non-200 status or a SOAP fault. A transport error
// says nothing about the path.
func (s *session) shouldDiscover(err error) bool {
	if err == nil || s.p.ServicePath != "" || s.eventPath != "" {
		return false
	}
	var st *statusError
	var f *faultError
	return errors.As(err, &st) || errors.As(err, &f)
}

// discover asks the device service where the event service is, with
// GetCapabilities and then GetServices, and reports whether that is a path
// other than the one in use. The XAddr gives only a path: the host and port
// stay the configured ones, as the requests carry credentials.
func (s *session) discover(ctx context.Context) bool {
	dev := s.onHost(deviceServicePath)
	var path string
	raw, err := soapCall(ctx, s.client, dev, capabilitiesBody, s.token())
	if err == nil {
		path, err = parseCapabilitiesEvents(raw)
	}
	if err != nil {
		slog.Debug("onvif GetCapabilities failed", "camera", s.p.Camera, "err", err)
		raw, err = soapCall(ctx, s.client, dev, servicesBody, s.token())
		if err == nil {
			path, err = parseServicesEvents(raw)
		}
	}
	if err != nil {
		slog.Warn("onvif event endpoint discovery failed", "camera", s.p.Camera, "err", err)
		return false
	}
	if path == s.servicePath() {
		return false
	}
	slog.Info("onvif event service discovered", "camera", s.p.Camera, "path", path)
	s.eventPath, s.devicePath = path, deviceServicePath
	return true
}

// onCamera keeps a subscription address's port and path but not its host:
// the address is the camera's reply, and requests carry credentials, so
// they go only to the configured host.
func (s *session) onCamera(address string) (string, error) {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad subscription address %q", address)
	}
	port := u.Port()
	if port == "" {
		port = strconv.Itoa(s.p.Port)
	}
	u.Scheme, u.User, u.Host = "http", nil, net.JoinHostPort(s.p.Host, port)
	return u.String(), nil
}

// follow pulls and renews a subscription until it fails or ctx is done.
// A failed Renew is the health signal; see pull.
func (s *session) follow(ctx context.Context, addr string, emit func(Signal)) error {
	pull := s.clk.NewTimer(pullEvery)
	defer pull.Stop()
	renew := s.clk.NewTimer(renewEvery)
	defer renew.Stop()
	sync := s.clk.NewTimer(clockSyncEvery)
	defer sync.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pull.C():
			if err := s.pull(ctx, addr, emit); err != nil {
				return fmt.Errorf("PullMessages: %w", err)
			}
			pull.Reset(pullEvery)
		case <-renew.C():
			if _, err := soapCall(ctx, s.client, addr, renewBody, s.token()); err != nil {
				return fmt.Errorf("renew: %w", err)
			}
			s.healthy = true
			renew.Reset(renewEvery)
		case <-sync.C():
			s.syncClock(ctx)
			sync.Reset(clockSyncEvery)
		}
	}
}

// pull emits the signals of one PullMessages reply.
//
// Tapo firmware does not hold PullMessages open until something happens. It
// drops the connection at once when nothing is queued, so a transport error
// here means "no events": it is counted and the same subscription is pulled
// again. A non-200 reply, a SOAP fault or an unreadable reply is a failure.
func (s *session) pull(ctx context.Context, addr string, emit func(Signal)) error {
	raw, err := soapCall(ctx, s.client, addr, pullBody, s.token())
	if te := (*transportError)(nil); errors.As(err, &te) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.metrics.PullRetry()
		return nil
	}
	if err != nil {
		return err
	}
	msgs, err := parsePull(raw)
	if err != nil {
		return err
	}
	s.healthy = true
	now := s.clk.Now()
	for _, m := range msgs {
		if s.p.tap != nil {
			s.p.tap(m)
		}
		matches := Match(s.p.Rules, m.Topic, m.Items)
		if len(matches) == 0 {
			s.metrics.Unmatched(m.Topic)
			slog.Debug("onvif event unmatched", "camera", s.p.Camera, "topic", m.Topic, "items", m.Items)
			continue
		}
		at := s.hostTime(m.UtcTime, now)
		for _, r := range matches {
			emit(Signal{Camera: s.p.Camera, Kind: r.Kind, Active: r.Active, At: at, RawTopic: m.Topic, Initialized: m.Op == "Initialized"})
		}
	}
	return nil
}

// hostTime moves a message UtcTime from camera to host time. A missing or
// unparsable time, or one in the future, gives now.
func (s *session) hostTime(utc string, now time.Time) time.Time {
	t, err := time.Parse(time.RFC3339, utc)
	if err != nil {
		return now
	}
	if t = t.Add(-s.offset).UTC(); t.After(now) {
		return now
	}
	return t
}

// unsubscribe frees the camera's limited subscription slot. It outlives ctx,
// as it runs on shutdown, and failure is harmless: the subscription lapses.
func (s *session) unsubscribe(addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	_, _ = soapCall(ctx, s.client, addr, unsubscribeBody, s.token())
}

// resetKinds forces every kind inactive, so an outage never claims a
// never-ending event.
func (s *session) resetKinds(emit func(Signal)) {
	kinds := slices.Clone(allKinds)
	for _, r := range s.p.Rules {
		if !slices.Contains(kinds, r.Kind) {
			kinds = append(kinds, r.Kind)
		}
	}
	now := s.clk.Now()
	for _, k := range kinds {
		emit(Signal{Camera: s.p.Camera, Kind: k, Active: false, At: now})
	}
}

type nopMetrics struct{}

func (nopMetrics) PullRetry()        {}
func (nopMetrics) Resubscribe()      {}
func (nopMetrics) Unmatched(string)  {}
func (nopMetrics) ClockSkew(float64) {}
