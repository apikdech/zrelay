package events

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponse caps how much of a camera reply is read (design §4.3).
const maxResponse = 1 << 20

const (
	nsEvents = "http://www.onvif.org/ver10/events/wsdl"
	nsDevice = "http://www.onvif.org/ver10/device/wsdl"
	nsWSN    = "http://docs.oasis-open.org/wsn/b-2"
)

// errTooLarge is a camera reply over maxResponse.
var errTooLarge = fmt.Errorf("camera reply over %d bytes", maxResponse)

// transportError is a request that got no complete HTTP reply.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// statusError is an HTTP reply other than 200.
type statusError struct {
	code   int
	reason string
}

func (e *statusError) Error() string { return fmt.Sprintf("camera answered %d: %s", e.code, e.reason) }

// faultError is a 200 reply carrying a SOAP fault.
type faultError struct{ reason string }

func (e *faultError) Error() string { return "camera answered a SOAP fault: " + e.reason }

// token is the WS-UsernameToken credential and the time to stamp it with.
type token struct {
	username, password string
	created            time.Time // camera time
}

// soapCall posts body in a SOAP envelope to address and returns the reply.
// A nil tok sends no WS-Security header.
func soapCall(ctx context.Context, client *http.Client, address, body string, tok *token) ([]byte, error) {
	envelope := buildEnvelope(address, body, tok)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(envelope))
	if err != nil {
		return nil, err
	}
	req.Close = true
	req.Header.Set("Connection", "close")
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	res, err := client.Do(req)
	if err != nil {
		return nil, &transportError{err}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return nil, &transportError{err}
	}
	if len(raw) > maxResponse {
		return nil, errTooLarge
	}
	if res.StatusCode != http.StatusOK {
		return nil, &statusError{res.StatusCode, faultReason(raw)}
	}
	var env struct {
		Fault *struct {
			Text string `xml:"Reason>Text"`
		} `xml:"Body>Fault"`
	}
	if xml.Unmarshal(raw, &env) == nil && env.Fault != nil {
		return nil, &faultError{faultReason(raw)}
	}
	return raw, nil
}

func faultReason(raw []byte) string {
	var env struct {
		Text string `xml:"Body>Fault>Reason>Text"`
	}
	if xml.Unmarshal(raw, &env) != nil || strings.TrimSpace(env.Text) == "" {
		return "no reason given"
	}
	r := strings.TrimSpace(env.Text)
	if len(r) > 200 {
		r = r[:200]
	}
	return r
}

// buildEnvelope wraps body in a SOAP 1.2 envelope with a WS-Addressing To
// and, when tok is set, a WS-UsernameToken password digest:
// Base64(SHA-1(nonce + created + password)).
func buildEnvelope(address, body string, tok *token) []byte {
	const (
		wsse = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"
		wsu  = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"
		wst  = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest"
		wsb  = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary"
	)
	security := ""
	if tok != nil {
		nonce := make([]byte, 16)
		rand.Read(nonce)
		created := tok.created.UTC().Format("2006-01-02T15:04:05Z")
		h := sha1.New()
		h.Write(nonce)
		h.Write([]byte(created))
		h.Write([]byte(tok.password))
		security = fmt.Sprintf(`<Security xmlns="%s"><UsernameToken><Username>%s</Username><Password Type="%s">%s</Password><Nonce EncodingType="%s">%s</Nonce><Created xmlns="%s">%s</Created></UsernameToken></Security>`,
			wsse, escapeXML(tok.username), wst, base64.StdEncoding.EncodeToString(h.Sum(nil)),
			wsb, base64.StdEncoding.EncodeToString(nonce), wsu, created)
	}
	return fmt.Appendf(nil, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://www.w3.org/2005/08/addressing"><s:Header>%s<a:To>%s</a:To></s:Header><s:Body>%s</s:Body></s:Envelope>`,
		security, escapeXML(address), body)
}

func escapeXML(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// parseSubscription returns the SubscriptionReference address of a
// CreatePullPointSubscription reply.
func parseSubscription(raw []byte) (string, error) {
	var env struct {
		Address string `xml:"Body>CreatePullPointSubscriptionResponse>SubscriptionReference>Address"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return "", err
	}
	if a := strings.TrimSpace(env.Address); a != "" {
		return a, nil
	}
	return "", errors.New("no subscription address in reply")
}

// parseCapabilitiesEvents returns the request path of the events XAddr of a
// GetCapabilities reply.
func parseCapabilitiesEvents(raw []byte) (string, error) {
	var env struct {
		XAddr string `xml:"Body>GetCapabilitiesResponse>Capabilities>Events>XAddr"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return "", err
	}
	return xaddrPath(env.XAddr)
}

// parseServicesEvents returns the request path of the events service XAddr
// of a GetServices reply.
func parseServicesEvents(raw []byte) (string, error) {
	var env struct {
		Services []struct {
			Namespace string `xml:"Namespace"`
			XAddr     string `xml:"XAddr"`
		} `xml:"Body>GetServicesResponse>Service"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return "", err
	}
	for _, sv := range env.Services {
		if strings.TrimSpace(sv.Namespace) == nsEvents {
			return xaddrPath(sv.XAddr)
		}
	}
	return "", errors.New("no events service in reply")
}

// xaddrPath returns the path and query of a service address. The host is
// dropped on purpose: the camera's own idea of its address is not trusted.
func xaddrPath(xaddr string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(xaddr))
	if err != nil || u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return "", fmt.Errorf("bad events address %q", xaddr)
	}
	return u.RequestURI(), nil
}

// parseSystemTime returns the camera UTC time of a GetSystemDateAndTime
// reply.
func parseSystemTime(raw []byte) (time.Time, error) {
	var env struct {
		UTC struct {
			Hour   int `xml:"Time>Hour"`
			Minute int `xml:"Time>Minute"`
			Second int `xml:"Time>Second"`
			Year   int `xml:"Date>Year"`
			Month  int `xml:"Date>Month"`
			Day    int `xml:"Date>Day"`
		} `xml:"Body>GetSystemDateAndTimeResponse>SystemDateAndTime>UTCDateTime"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return time.Time{}, err
	}
	u := env.UTC
	if u.Year == 0 || u.Month == 0 || u.Day == 0 {
		return time.Time{}, errors.New("no UTCDateTime in reply")
	}
	return time.Date(u.Year, time.Month(u.Month), u.Day, u.Hour, u.Minute, u.Second, 0, time.UTC), nil
}

// notification is one NotificationMessage of a PullMessages reply.
type notification struct {
	Topic   string
	UtcTime string
	Op      string // PropertyOperation
	Source  []Item // Message/Source SimpleItems
	Items   []Item // Message/Data SimpleItems
}

func parsePull(raw []byte) ([]notification, error) {
	var env struct {
		Notifications []struct {
			Topic   string `xml:"Topic"`
			Message struct {
				UtcTime string `xml:"UtcTime,attr"`
				Op      string `xml:"PropertyOperation,attr"`
				Source  []struct {
					Name  string `xml:"Name,attr"`
					Value string `xml:"Value,attr"`
				} `xml:"Source>SimpleItem"`
				Items []struct {
					Name  string `xml:"Name,attr"`
					Value string `xml:"Value,attr"`
				} `xml:"Data>SimpleItem"`
			} `xml:"Message>Message"`
		} `xml:"Body>PullMessagesResponse>NotificationMessage"`
	}
	if err := xml.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	out := make([]notification, 0, len(env.Notifications))
	for _, n := range env.Notifications {
		items := make([]Item, len(n.Message.Items))
		for i, it := range n.Message.Items {
			items[i] = Item{Name: it.Name, Value: it.Value}
		}
		source := make([]Item, len(n.Message.Source))
		for i, it := range n.Message.Source {
			source[i] = Item{Name: it.Name, Value: it.Value}
		}
		out = append(out, notification{Topic: strings.TrimSpace(n.Topic), UtcTime: n.Message.UtcTime, Op: n.Message.Op, Source: source, Items: items})
	}
	return out, nil
}
