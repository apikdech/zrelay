package camera

import (
	"sync"
	"sync/atomic"
)

// EventKind says which field of an Event is set.
type EventKind int

// Event kinds.
const (
	// EventCodec carries the session's codec (Event.Codec, Event.TimeSource).
	EventCodec EventKind = iota
	// EventVideo carries one access unit (Event.AU).
	EventVideo
	// EventAudio carries one G.711 packet (Event.Audio).
	EventAudio
	// EventLost says the stream is broken for this subscriber: the session
	// was lost, or the subscriber lagged and its queue was cleared.
	EventLost
)

// Event is one item a Hub delivers. AU, Audio and Codec are shared by every
// subscriber and must not be modified.
type Event struct {
	Kind       EventKind
	Codec      *Codec
	TimeSource TimeSource
	AU         *AccessUnit
	Audio      *AudioPacket
}

// Class is the kind of subscriber (design §4.1).
type Class int

// Subscriber classes.
const (
	// ClassRecorder receives codec, video and loss events, never audio.
	ClassRecorder Class = iota
	// ClassViewer (live view, restream) also receives audio.
	ClassViewer
)

// minQueue leaves room for EventLost and the EventCodec that follows it
// after a lag.
const minQueue = 2

// Hub fans the output of one camera's session out to subscribers. It
// implements SessionSink. Publishing never blocks: a subscriber whose queue
// is full is lagged (see Subscribe).
type Hub struct {
	camera string

	mu       sync.Mutex
	subs     map[*Sub]struct{}
	codec    *Codec
	ts       TimeSource
	hasCodec bool
}

// NewHub returns a Hub for the named camera.
func NewHub(camera string) *Hub {
	return &Hub{camera: camera, subs: make(map[*Sub]struct{})}
}

// Camera returns the camera name the Hub was created with.
func (h *Hub) Camera() string { return h.camera }

// Sub is one subscription to a Hub.
type Sub struct {
	hub     *Hub
	class   Class
	ch      chan Event
	lagged  atomic.Uint64
	waitIDR bool // guarded by hub.mu
	closed  bool // guarded by hub.mu
}

// Subscribe registers a subscriber with a queue of the given size (at
// least 2). If the Hub knows the current codec, it is the first event. Video
// starts at the next IDR.
//
// When a delivery would block, the Hub clears the subscriber's queue,
// increments Lagged, delivers one EventLost followed by the current codec,
// and skips video until the next IDR. Audio resumes at once for viewers.
// Consumers must therefore expect an EventCodec right after an EventLost
// even when the codec has not changed. A full queue met by Lost is cleared
// the same way but does not count as lag.
func (h *Hub) Subscribe(class Class, queue int) *Sub {
	s := &Sub{hub: h, class: class, ch: make(chan Event, max(queue, minQueue)), waitIDR: true}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hasCodec {
		s.ch <- Event{Kind: EventCodec, Codec: h.codec, TimeSource: h.ts}
	}
	h.subs[s] = struct{}{}
	return s
}

// C returns the subscriber's events. It is closed by Close.
func (s *Sub) C() <-chan Event { return s.ch }

// Lagged returns how many times the subscriber's queue overflowed.
func (s *Sub) Lagged() uint64 { return s.lagged.Load() }

// Close unsubscribes and closes C. It is safe to call more than once.
func (s *Sub) Close() {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	delete(h.subs, s)
	close(s.ch)
}

// CurrentCodec returns the current codec and time source, for late
// subscribers. ok is false before the first codec and after Lost. (The plan
// names it Codec, but that name is taken by SessionSink.Codec.)
func (h *Hub) CurrentCodec() (c Codec, ts TimeSource, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.hasCodec {
		return Codec{}, "", false
	}
	return *h.codec, h.ts, true
}

// Codec records c as the current codec (SessionSink) and broadcasts it.
func (h *Hub) Codec(c Codec, ts TimeSource) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.codec, h.ts, h.hasCodec = &c, ts, true
	ev := Event{Kind: EventCodec, Codec: &c, TimeSource: ts}
	for s := range h.subs {
		h.deliver(s, ev)
	}
}

// Video broadcasts au (SessionSink). Subscribers waiting for an IDR skip it
// unless au.IDR.
func (h *Hub) Video(au *AccessUnit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := Event{Kind: EventVideo, AU: au}
	for s := range h.subs {
		if s.waitIDR {
			if !au.IDR {
				continue
			}
			s.waitIDR = false
		}
		h.deliver(s, ev)
	}
}

// Audio broadcasts p to viewers (SessionSink).
func (h *Hub) Audio(p *AudioPacket) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := Event{Kind: EventAudio, Audio: p}
	for s := range h.subs {
		if s.class == ClassViewer {
			h.deliver(s, ev)
		}
	}
}

// Lost broadcasts EventLost: the session ended. The current codec is
// forgotten and every subscriber resumes video at the next IDR.
func (h *Hub) Lost() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.codec, h.ts, h.hasCodec = nil, "", false
	for s := range h.subs {
		s.waitIDR = true
		h.deliver(s, Event{Kind: EventLost})
	}
}

// deliver sends ev to s without blocking, lagging s if its queue is full.
// h.mu must be held.
func (h *Hub) deliver(s *Sub, ev Event) {
	select {
	case s.ch <- ev:
		return
	default:
	}
	// Lagged: clear the queue, then resync. The queue is empty and only the
	// Hub sends, so the sends below cannot block. ev is dropped: a video
	// event is replaced by waiting for the next IDR, a lost event by the
	// EventLost below, and a codec event (now current) by the re-sent codec.
	for drained := false; !drained; {
		select {
		case <-s.ch:
		default:
			drained = true
		}
	}
	if ev.Kind != EventLost {
		// A session loss (Lost) on a full queue is not lag: the subscriber
		// gets the same single EventLost, but Lagged, which feeds
		// zrelay_recorder_lag_total, counts only slow consumers.
		s.lagged.Add(1)
	}
	s.waitIDR = true
	s.ch <- Event{Kind: EventLost}
	if h.hasCodec {
		s.ch <- Event{Kind: EventCodec, Codec: h.codec, TimeSource: h.ts}
	}
}
