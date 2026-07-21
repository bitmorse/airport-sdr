package receiver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bitmorse/airport-sdr/internal/config"
	"github.com/bitmorse/airport-sdr/internal/sdr"
	"github.com/bitmorse/airport-sdr/internal/stream"
)

// --- a stub radio -----------------------------------------------------------

// stubSource stands in for a radio. It emits empty blocks as fast as the
// consumer takes them and records every retune, which is enough to observe the
// receiver's whole lifecycle without any hardware.
type stubSource struct {
	mu        sync.Mutex
	center    float64
	rate      float64
	streaming bool
	// retunedWhileStreaming records a violation of the Source contract: the
	// device must never be moved out from under a running receive loop.
	retunedWhileStreaming bool
	retunes               [][2]float64
	failRetune            error
}

func newStub(center, rate float64) *stubSource {
	return &stubSource{center: center, rate: rate}
}

func (s *stubSource) SampleRate() float64 { return s.rate }
func (s *stubSource) CenterFreq() float64 { return s.center }
func (s *stubSource) Describe() string    { return "stub source" }
func (s *stubSource) Close() error        { return nil }

func (s *stubSource) Retune(center, rate float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.streaming {
		s.retunedWhileStreaming = true
	}
	if s.failRetune != nil {
		return s.failRetune
	}
	s.retunes = append(s.retunes, [2]float64{center, rate})
	s.center, s.rate = center, rate
	return nil
}

func (s *stubSource) Start(ctx context.Context) (<-chan *sdr.Block, error) {
	s.mu.Lock()
	s.streaming = true
	blockSize := sdr.BlockSizeFor(s.rate)
	s.mu.Unlock()

	out := make(chan *sdr.Block)
	pool := sdr.NewBlockPool(4, blockSize)

	go func() {
		defer close(out)
		defer func() {
			s.mu.Lock()
			s.streaming = false
			s.mu.Unlock()
		}()

		for ctx.Err() == nil {
			b, ok := pool.Get()
			if !ok {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Millisecond):
				}
				continue
			}
			select {
			case out <- b:
			case <-ctx.Done():
				b.Release()
				return
			}
		}
	}()
	return out, nil
}

func (s *stubSource) snapshot() (retunes [][2]float64, violated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]float64(nil), s.retunes...), s.retunedWhileStreaming
}

// --- fixtures ---------------------------------------------------------------

// twoGroups mirrors the real layout: one group per tuner position, each with
// its own channels.
func twoGroups() config.Config {
	c := config.Default()
	c.Groups = []config.GroupConfig{
		{
			Name: "Alpha", CenterFreq: 118_250_000, SampleRate: 960_000,
			Channels: []config.ChannelConfig{
				{Name: "Tower", Freq: 118_100_000, Mode: config.ModeAM, SquelchDB: -62},
			},
		},
		{
			Name: "Bravo", CenterFreq: 121_805_000, SampleRate: 960_000,
			Channels: []config.ChannelConfig{
				{Name: "Ground", Freq: 121_902_000, Mode: config.ModeAM, SquelchDB: -62},
				{Name: "Delivery", Freq: 121_925_000, Mode: config.ModeAM, SquelchDB: -62},
			},
		},
	}
	if err := c.Validate(); err != nil {
		panic("test fixture is invalid: " + err.Error())
	}
	return c
}

func newTestReceiver(t *testing.T) (*Receiver, *stubSource) {
	t.Helper()
	cfg := twoGroups()
	src := newStub(cfg.Groups[0].CenterFreq, cfg.Groups[0].SampleRate)

	r, err := New(cfg, src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, src
}

// run starts the receiver and stops it when the test ends.
func run(t *testing.T, r *Receiver) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancellation")
		}
	})
	return ctx
}

func hubOf(t *testing.T, r *Receiver, name string) *stream.Hub {
	t.Helper()
	for _, ch := range r.Channels() {
		if ch.Name == name {
			return ch.Hub
		}
	}
	t.Fatalf("no channel named %q", name)
	return nil
}

// receivesWithin reports whether any audio arrives for a subscriber.
func receivesWithin(sub *stream.Subscriber, d time.Duration) bool {
	select {
	case _, ok := <-sub.Frames():
		return ok
	case <-time.After(d):
		return false
	}
}

// --- construction -----------------------------------------------------------

// Every channel gets a hub up front, including channels in groups that are not
// currently tuned. That is what lets a switch change which chains run without
// disturbing the set of endpoints, so no listener is disconnected.
func TestNewCreatesAHubForEveryChannelInEveryGroup(t *testing.T) {
	r, _ := newTestReceiver(t)

	channels := r.Channels()
	if len(channels) != 3 {
		t.Fatalf("got %d channels, want 3 across both groups", len(channels))
	}
	for _, ch := range channels {
		if ch.Hub == nil {
			t.Errorf("channel %q has no hub", ch.Name)
		}
		if ch.Group == "" {
			t.Errorf("channel %q does not report its group", ch.Name)
		}
	}
}

func TestNewStartsOnTheFirstGroup(t *testing.T) {
	r, _ := newTestReceiver(t)
	if got := r.ActiveGroup(); got != "Alpha" {
		t.Errorf("ActiveGroup = %q, want the first configured group", got)
	}
}

func TestNewRejectsAConfigWithNoGroups(t *testing.T) {
	cfg := twoGroups()
	cfg.Groups = nil
	if _, err := New(cfg, newStub(118e6, 960_000)); err == nil {
		t.Fatal("a receiver with no groups should be an error")
	}
}

// --- running ----------------------------------------------------------------

func TestOnlyTheActiveGroupsChannelsReceiveAudio(t *testing.T) {
	r, _ := newTestReceiver(t)

	active := hubOf(t, r, "Tower").Subscribe()
	defer active.Close()
	idle := hubOf(t, r, "Ground").Subscribe()
	defer idle.Close()

	run(t, r)

	if !receivesWithin(active, 3*time.Second) {
		t.Error("the active group's channel received no audio")
	}
	if receivesWithin(idle, 300*time.Millisecond) {
		t.Error("a channel in an inactive group received audio")
	}
}

func TestRunReturnsWhenContextIsCancelled(t *testing.T) {
	r, _ := newTestReceiver(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation")
	}
}

// --- switching --------------------------------------------------------------

func TestSwitchMovesAudioToTheNewGroup(t *testing.T) {
	r, src := newTestReceiver(t)

	alpha := hubOf(t, r, "Tower").Subscribe()
	defer alpha.Close()
	bravo := hubOf(t, r, "Ground").Subscribe()
	defer bravo.Close()

	ctx := run(t, r)
	if !receivesWithin(alpha, 3*time.Second) {
		t.Fatal("no audio on the initial group")
	}

	if err := r.Switch(ctx, "Bravo"); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if got := r.ActiveGroup(); got != "Bravo" {
		t.Errorf("ActiveGroup = %q after switching, want Bravo", got)
	}

	if !receivesWithin(bravo, 3*time.Second) {
		t.Error("the newly active group received no audio")
	}

	// Drain anything queued before the switch, then confirm the old group has
	// genuinely stopped rather than merely being behind. The drain is bounded:
	// if the old group never stops, this must fail rather than hang.
	drained := 0
	for receivesWithin(alpha, 100*time.Millisecond) {
		if drained++; drained > hubDepth+4 {
			t.Fatal("the previous group is still producing audio after the switch")
		}
	}
	if receivesWithin(alpha, 500*time.Millisecond) {
		t.Error("the previous group is still producing audio after the switch")
	}

	retunes, _ := src.snapshot()
	if len(retunes) != 1 {
		t.Fatalf("got %d retunes, want exactly 1", len(retunes))
	}
	if retunes[0] != [2]float64{121_805_000, 960_000} {
		t.Errorf("retuned to %v, want the Bravo group's tuning", retunes[0])
	}
}

// The Source contract forbids retuning a live stream. This is the test that
// keeps the sequential handoff honest.
func TestSwitchStopsTheStreamBeforeRetuning(t *testing.T) {
	r, src := newTestReceiver(t)
	ctx := run(t, r)

	time.Sleep(100 * time.Millisecond)
	if err := r.Switch(ctx, "Bravo"); err != nil {
		t.Fatalf("Switch: %v", err)
	}

	if _, violated := src.snapshot(); violated {
		t.Error("the source was retuned while its stream was still running")
	}
}

func TestSwitchToAnUnknownGroupFails(t *testing.T) {
	r, _ := newTestReceiver(t)
	ctx := run(t, r)

	err := r.Switch(ctx, "Nowhere")
	if err == nil {
		t.Fatal("switching to an unknown group must fail")
	}
	if got := r.ActiveGroup(); got != "Alpha" {
		t.Errorf("ActiveGroup = %q after a failed switch, want it unchanged", got)
	}
}

func TestSwitchToTheActiveGroupIsANoOp(t *testing.T) {
	r, src := newTestReceiver(t)
	ctx := run(t, r)

	if err := r.Switch(ctx, "Alpha"); err != nil {
		t.Fatalf("switching to the already-active group should succeed: %v", err)
	}
	if retunes, _ := src.snapshot(); len(retunes) != 0 {
		t.Errorf("got %d retunes, want none for a no-op switch", len(retunes))
	}
}

// A radio that refuses the new tuning must leave the receiver working on the
// old one, not dead. This is the difference between a failed switch costing a
// message and costing the whole service.
func TestFailedRetuneKeepsThePreviousGroupRunning(t *testing.T) {
	r, src := newTestReceiver(t)

	alpha := hubOf(t, r, "Tower").Subscribe()
	defer alpha.Close()

	ctx := run(t, r)
	if !receivesWithin(alpha, 3*time.Second) {
		t.Fatal("no audio before the switch")
	}

	src.mu.Lock()
	src.failRetune = errors.New("device says no")
	src.mu.Unlock()

	if err := r.Switch(ctx, "Bravo"); err == nil {
		t.Fatal("Switch must report the retune failure")
	}
	if got := r.ActiveGroup(); got != "Alpha" {
		t.Errorf("ActiveGroup = %q after a failed retune, want Alpha", got)
	}
	if !receivesWithin(alpha, 3*time.Second) {
		t.Error("the previous group stopped producing audio after a failed retune")
	}
}

func TestChannelStateReportsSquelchAndLevel(t *testing.T) {
	r, _ := newTestReceiver(t)
	run(t, r)
	time.Sleep(300 * time.Millisecond)

	for _, ch := range r.Channels() {
		if ch.Name != "Tower" {
			continue
		}
		state := ch.State()
		// The stub emits silence, so the channel must be shut and its level at
		// the floor rather than at the zero value.
		if state.SquelchOpen {
			t.Error("squelch reported open on a silent stub source")
		}
		if state.LevelDB > -100 {
			t.Errorf("level = %v dBFS on silence, want a floor value", state.LevelDB)
		}
	}
}
