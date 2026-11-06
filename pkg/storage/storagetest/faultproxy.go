package storagetest

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

// FaultProxy is a TCP proxy in front of a storage endpoint that injects
// network faults: a connection cut after a byte budget, and a blackhole.
type FaultProxy struct {
	ln       net.Listener
	upstream string // host:port

	mu        sync.Mutex
	killAfter int64 // bytes left before the cut; < 0 when disarmed
	blackhole bool
	conns     map[*proxyConn]struct{}
	closed    bool // close has run; new connections are refused
	wg        sync.WaitGroup
}

type proxyConn struct {
	client, server net.Conn
	tainted        bool // dropped bytes while blackholed; unusable afterwards
	pumps          int  // pumps that have ended
}

func (c *proxyConn) close() {
	_ = c.client.Close()
	_ = c.server.Close()
}

// StartFaultProxy listens on 127.0.0.1:0 and forwards every connection to
// upstream, given as host:port or as an http(s):// URL. It is closed when the
// test ends.
func StartFaultProxy(t *testing.T, upstream string) *FaultProxy {
	t.Helper()
	if _, rest, ok := strings.Cut(upstream, "://"); ok {
		upstream, _, _ = strings.Cut(rest, "/")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fault proxy: listen: %v", err)
	}
	p := &FaultProxy{ln: ln, upstream: upstream, killAfter: -1, conns: map[*proxyConn]struct{}{}}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(p.close)
	return p
}

// URL is the proxy's base URL, for use as an S3 endpoint.
func (p *FaultProxy) URL() string { return "http://" + p.ln.Addr().String() }

// KillAfterBytes arms a one-shot cut: once n more bytes have been forwarded
// upstream (counted across connections, so it also hits a reused keep-alive
// connection), the connection carrying the next byte is closed on both sides
// after forwarding exactly those n bytes.
func (p *FaultProxy) KillAfterBytes(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killAfter = max(n, 0)
}

// Blackhole, while on, silently drops every byte in both directions without
// closing anything, as a network partition does. Turning it off closes the
// connections that lost bytes, so clients see a reset rather than a stream
// with a hole in it; new connections then flow normally.
func (p *FaultProxy) Blackhole(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blackhole = on
	if on {
		return
	}
	for c := range p.conns {
		if c.tainted {
			c.close()
			delete(p.conns, c)
		}
	}
}

func (p *FaultProxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		c := &proxyConn{client: client, server: server}
		if !p.track(c) {
			continue
		}
		go p.pump(c, client, server, true)
		go p.pump(c, server, client, false)
	}
}

// track registers c and reserves its two pumps in p.wg. Once close has run it
// closes c instead and returns false: close has already walked p.conns, so a
// connection registered afterwards would never be closed.
func (p *FaultProxy) track(c *proxyConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		c.close()
		return false
	}
	p.conns[c] = struct{}{}
	p.wg.Add(2)
	return true
}

// pump copies src to dst. Upstream-bound bytes are charged against the kill
// budget. On EOF it half-closes dst so the peer can still answer; on any other
// end (error or cut) it closes both sides.
func (p *FaultProxy) pump(c *proxyConn, src, dst net.Conn, upstream bool) {
	defer p.wg.Done()
	full := true
	defer func() { p.done(c, full) }()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk, cut := p.admit(c, buf[:n], upstream)
			if len(chunk) > 0 {
				if _, werr := dst.Write(chunk); werr != nil {
					return
				}
			}
			if cut {
				return
			}
		}
		if errors.Is(err, io.EOF) {
			if tc, ok := dst.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
				full = false
			}
			return
		}
		if err != nil {
			return
		}
	}
}

// admit decides what part of chunk to forward, and whether to cut c after it.
func (p *FaultProxy) admit(c *proxyConn, chunk []byte, upstream bool) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.blackhole {
		c.tainted = true
		return nil, false
	}
	if !upstream || p.killAfter < 0 {
		return chunk, false
	}
	if int64(len(chunk)) <= p.killAfter {
		p.killAfter -= int64(len(chunk))
		return chunk, false
	}
	chunk = chunk[:p.killAfter]
	p.killAfter = -1
	return chunk, true
}

// done records the end of one of c's two pumps. c is closed on both sides
// when a pump ends abnormally or when both have ended.
func (p *FaultProxy) done(c *proxyConn, full bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c.pumps++
	if full || c.pumps == 2 {
		c.close()
		delete(p.conns, c)
	}
}

func (p *FaultProxy) close() {
	_ = p.ln.Close()
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		c.close()
		delete(p.conns, c)
	}
	p.mu.Unlock()
	p.wg.Wait()
}
