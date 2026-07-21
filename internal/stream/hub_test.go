package stream

import (
	"sync"
	"testing"
	"time"
)

func recvWithin(t *testing.T, sub *Subscriber, d time.Duration) []byte {
	t.Helper()
	select {
	case frame, ok := <-sub.Frames():
		if !ok {
			t.Fatal("subscriber channel closed unexpectedly")
		}
		return frame
	case <-time.After(d):
		t.Fatal("timed out waiting for a frame")
		return nil
	}
}

func TestHubDeliversToSubscriber(t *testing.T) {
	h := NewHub(4, 8)
	sub := h.Subscribe()
	defer sub.Close()

	h.Publish([]byte{1, 2, 3})

	got := recvWithin(t, sub, time.Second)
	if string(got) != string([]byte{1, 2, 3}) {
		t.Errorf("got % x, want 01 02 03", got)
	}
}

// The DSP chain reuses its output buffer on every block, so the hub must copy.
// Without this, a listener would receive whatever the chain happened to be
// holding by the time the frame was written to the socket.
func TestHubCopiesPublishedData(t *testing.T) {
	h := NewHub(4, 8)
	sub := h.Subscribe()
	defer sub.Close()

	shared := []byte{1, 2, 3}
	h.Publish(shared)
	for i := range shared {
		shared[i] = 0xFF // the chain moves on and overwrites its buffer
	}

	got := recvWithin(t, sub, time.Second)
	if string(got) != string([]byte{1, 2, 3}) {
		t.Errorf("got % x, want the data as it was at publish time", got)
	}
}

func TestHubDeliversToEverySubscriber(t *testing.T) {
	h := NewHub(4, 8)
	subs := make([]*Subscriber, 3)
	for i := range subs {
		subs[i] = h.Subscribe()
		defer subs[i].Close()
	}

	h.Publish([]byte{42})

	for i, sub := range subs {
		if got := recvWithin(t, sub, time.Second); len(got) != 1 || got[0] != 42 {
			t.Errorf("subscriber %d got % x, want 2a", i, got)
		}
	}
}

func TestHubCountsListeners(t *testing.T) {
	h := NewHub(4, 8)
	if h.Listeners() != 0 {
		t.Fatalf("new hub has %d listeners, want 0", h.Listeners())
	}

	a, b := h.Subscribe(), h.Subscribe()
	if h.Listeners() != 2 {
		t.Errorf("after two Subscribe calls: %d listeners, want 2", h.Listeners())
	}

	a.Close()
	if h.Listeners() != 1 {
		t.Errorf("after one Close: %d listeners, want 1", h.Listeners())
	}
	b.Close()
	if h.Listeners() != 0 {
		t.Errorf("after both closed: %d listeners, want 0", h.Listeners())
	}
}

// The whole point of the hub. A listener on a bad connection must never be able
// to stall the receive chain: audio is real-time, so the only correct response
// to a listener who cannot keep up is to drop frames for that listener alone.
func TestHubNeverBlocksOnASlowSubscriber(t *testing.T) {
	h := NewHub(2, 8)
	slow := h.Subscribe() // never reads
	defer slow.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			h.Publish([]byte{byte(i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that was not reading")
	}
	if h.Dropped() == 0 {
		t.Error("frames were dropped for the slow subscriber but not counted")
	}
}

// A listener who cannot keep up must not degrade anyone else's audio.
func TestHubSlowSubscriberDoesNotAffectFastOne(t *testing.T) {
	h := NewHub(2, 8)
	slow := h.Subscribe() // deliberately never read
	defer slow.Close()
	fast := h.Subscribe()
	defer fast.Close()

	var received int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range fast.Frames() {
			if received++; received == 50 {
				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		h.Publish([]byte{byte(i)})
		time.Sleep(time.Millisecond)
	}
	wg.Wait()

	if received != 50 {
		t.Errorf("fast subscriber got %d of 50 frames", received)
	}
}

func TestHubPublishWithNoSubscribers(t *testing.T) {
	h := NewHub(4, 8)
	h.Publish([]byte{1, 2, 3}) // must not panic or block
	if h.Listeners() != 0 {
		t.Errorf("listeners = %d, want 0", h.Listeners())
	}
}

func TestHubClosedSubscriberStopsReceiving(t *testing.T) {
	h := NewHub(4, 8)
	sub := h.Subscribe()
	sub.Close()

	h.Publish([]byte{1})

	// The channel must be closed, not merely idle, so a reader's range ends.
	select {
	case _, ok := <-sub.Frames():
		if ok {
			t.Error("a closed subscriber still received a frame")
		}
	case <-time.After(time.Second):
		t.Error("channel of a closed subscriber was never closed")
	}
}

func TestHubDoubleCloseIsSafe(t *testing.T) {
	h := NewHub(4, 8)
	sub := h.Subscribe()
	sub.Close()
	sub.Close() // must not panic on a double close

	if h.Listeners() != 0 {
		t.Errorf("listeners = %d, want 0", h.Listeners())
	}
}

// Subscribers come and go while the radio is running, so the hub is used
// concurrently by definition. Run under -race.
func TestHubConcurrentSubscribeAndPublish(t *testing.T) {
	h := NewHub(4, 8)
	stop := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.Publish([]byte{1, 2, 3, 4})
			}
		}
	}()

	for i := 0; i < 50; i++ {
		sub := h.Subscribe()
		go func() {
			for range sub.Frames() { //nolint:revive // draining
			}
		}()
		sub.Close()
	}

	close(stop)
	wg.Wait()
}

// Publishing runs for every audio block for as long as the receiver is up, so
// it must not add to the garbage collector's workload.
func TestAllocHubPublish(t *testing.T) {
	h := NewHub(8, 64)
	sub := h.Subscribe()
	defer sub.Close()

	go func() {
		for range sub.Frames() { //nolint:revive // draining
		}
	}()

	frame := make([]byte, 64)
	time.Sleep(10 * time.Millisecond)

	if n := testing.AllocsPerRun(100, func() {
		h.Publish(frame)
	}); n != 0 {
		t.Errorf("Hub.Publish allocated %v times per run, want 0", n)
	}
}
