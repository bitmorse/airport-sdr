package stream

import (
	"sync"
	"sync/atomic"
)

// Hub fans one channel's audio out to every listener.
//
// The rule it exists to enforce: publishing must never block. Audio is
// real-time and the radio cannot pause, so a listener on a bad connection gets
// frames dropped rather than being allowed to stall the receive chain or grow
// the heap. Each subscriber therefore owns a fixed ring of preallocated frames,
// which also keeps publishing free of allocation.
type Hub struct {
	mu      sync.RWMutex
	subs    map[*Subscriber]struct{}
	depth   int // frames buffered per subscriber
	size    int // bytes per frame
	dropped atomic.Uint64
}

// NewHub creates a hub whose subscribers each buffer depth frames of up to
// size bytes. Depth trades latency against tolerance of a jittery connection:
// a few frames is usually right, since stale audio is worthless anyway.
func NewHub(depth, size int) *Hub {
	if depth < 1 {
		depth = 1
	}
	return &Hub{subs: make(map[*Subscriber]struct{}), depth: depth, size: size}
}

// Subscriber receives frames from a Hub until closed.
type Subscriber struct {
	hub *Hub
	ch  chan []byte

	// ring holds this subscriber's own frame buffers, so publishing copies
	// into memory nobody else is reading and never allocates.
	ring [][]byte
	next int

	closeOnce sync.Once
}

// Subscribe adds a listener. The caller must Close it when finished.
func (h *Hub) Subscribe() *Subscriber {
	// One more buffer than the channel can hold: at most depth frames are in
	// flight, leaving one free to write into.
	ring := make([][]byte, h.depth+1)
	for i := range ring {
		ring[i] = make([]byte, h.size)
	}

	sub := &Subscriber{hub: h, ch: make(chan []byte, h.depth), ring: ring}

	h.mu.Lock()
	h.subs[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

// Frames returns the channel of audio frames, closed when the subscriber is.
func (s *Subscriber) Frames() <-chan []byte { return s.ch }

// Close detaches the subscriber and closes its channel, so a reader ranging
// over Frames terminates. It is safe to call more than once.
func (s *Subscriber) Close() {
	s.closeOnce.Do(func() {
		s.hub.mu.Lock()
		delete(s.hub.subs, s)
		s.hub.mu.Unlock()
		close(s.ch)
	})
}

// Publish sends a frame to every listener. It copies, because the caller's
// buffer belongs to the DSP chain and is overwritten on the next block.
func (h *Hub) Publish(frame []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for sub := range h.subs {
		if !sub.deliver(frame) {
			h.dropped.Add(1)
		}
	}
}

// deliver copies frame into the subscriber's next free buffer and queues it,
// reporting false when the listener is too far behind to accept it.
func (s *Subscriber) deliver(frame []byte) bool {
	buf := s.ring[s.next]
	if len(frame) > cap(buf) {
		// Larger than expected: fall back to a fresh buffer rather than
		// truncating audio. Not the steady-state path.
		buf = make([]byte, len(frame))
	}
	buf = buf[:len(frame)]
	copy(buf, frame)

	select {
	case s.ch <- buf:
		// Only advance once the frame is safely queued, so a dropped frame
		// reuses the same buffer instead of overwriting one still in flight.
		s.next = (s.next + 1) % len(s.ring)
		return true
	default:
		return false
	}
}

// Listeners reports how many subscribers are attached.
func (h *Hub) Listeners() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Dropped counts frames discarded because a listener could not keep up. A
// steadily rising count means a listener's connection is too slow, not that the
// receiver is at fault.
func (h *Hub) Dropped() uint64 { return h.dropped.Load() }
