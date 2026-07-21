// Package receiver ties a radio to the DSP chains and the listeners.
//
// A single tuner covers one slice of spectrum at a time, but every channel
// inside that slice is demodulated in parallel. Channels are therefore grouped
// by the tuning that covers them, and the receiver switches the radio between
// groups on request.
//
// Two decisions shape everything here:
//
//   - Hubs and DSP chains exist for every channel in every group, built once at
//     construction. Switching changes only which chains are fed, so the set of
//     endpoints never changes, no listener is disconnected, and a switch
//     allocates nothing.
//   - Switching is a sequential handoff rather than shared mutable state: the
//     stream is stopped, the radio retuned, and the stream restarted, all from
//     the same goroutine. Nothing on the audio path needs a lock.
package receiver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"

	"github.com/bitmorse/airport-sdr/internal/config"
	"github.com/bitmorse/airport-sdr/internal/dsp"
	"github.com/bitmorse/airport-sdr/internal/sdr"
	"github.com/bitmorse/airport-sdr/internal/stream"
)

// hubDepth is how many audio frames a listener may fall behind before frames
// are dropped for them. At 20 ms a frame this is a fraction of a second: enough
// to ride out ordinary network jitter, little enough that nobody ends up
// listening to stale traffic.
const hubDepth = 8

// tuningTolerance is how closely a source's tuning must match a group's for the
// two to be considered the same, in hertz.
const tuningTolerance = 1.0

// ChannelState is a live snapshot of one channel.
type ChannelState struct {
	LevelDB     float64
	SquelchOpen bool
}

// Channel is the receiver's public handle on one configured channel.
type Channel struct {
	Name      string
	Group     string
	Freq      float64
	AudioRate int
	Hub       *stream.Hub
	State     func() ChannelState
}

// GroupInfo describes one tuner position for the API.
type GroupInfo struct {
	Name       string
	CenterFreq float64
	SampleRate float64
	Channels   []string
	Active     bool
}

// channelRuntime is one channel's DSP chain and its listeners.
type channelRuntime struct {
	cfg   config.ChannelConfig
	group string
	dsp   *dsp.Channel
	hub   *stream.Hub

	frame []byte // reused mu-law scratch, so the audio path never allocates

	// The receive loop writes these while HTTP handlers read them, so they are
	// atomics rather than mutex-guarded state. That keeps listener traffic off
	// the audio path entirely.
	levelBits atomic.Uint64
	open      atomic.Bool
}

func (c *channelRuntime) state() ChannelState {
	return ChannelState{
		LevelDB:     math.Float64frombits(c.levelBits.Load()),
		SquelchOpen: c.open.Load(),
	}
}

type groupRuntime struct {
	cfg      config.GroupConfig
	channels []*channelRuntime
}

// switchRequest asks the receive loop to move to another group.
type switchRequest struct {
	group string
	reply chan error
}

// Receiver runs the radio and the DSP for the active group.
type Receiver struct {
	cfg config.Config
	src sdr.Source

	groups   []*groupRuntime
	handles  []Channel
	active   atomic.Pointer[groupRuntime]
	switches chan *switchRequest
}

// New builds the chains for every group and selects the one the source is
// already tuned to.
//
// The source must have been opened on one of the configured groups; a mismatch
// is a wiring mistake and is reported rather than silently demodulating the
// wrong offsets from the right samples.
func New(cfg config.Config, src sdr.Source) (*Receiver, error) {
	if len(cfg.Groups) == 0 {
		return nil, errors.New("the receiver needs at least one channel group")
	}

	r := &Receiver{cfg: cfg, src: src, switches: make(chan *switchRequest)}
	if err := r.buildGroups(); err != nil {
		return nil, err
	}

	active, err := r.groupMatchingSource()
	if err != nil {
		return nil, err
	}
	r.active.Store(active)
	return r, nil
}

// buildGroups constructs every group's chains, hubs and status handles up
// front, so that switching later costs no allocation.
func (r *Receiver) buildGroups() error {
	for _, g := range r.cfg.Groups {
		block := sdr.BlockSizeFor(g.SampleRate)
		// Audio samples per input block, plus slack for the filter boundaries.
		maxAudio := int(float64(block)*r.cfg.Audio.Rate/g.SampleRate) + 8

		runtime := &groupRuntime{cfg: g}
		for _, ch := range g.Channels {
			chain, err := dsp.NewChannel(dsp.ChannelOptions{
				Name:            ch.Name,
				Offset:          ch.Freq - g.CenterFreq,
				InputRate:       g.SampleRate,
				AudioRate:       r.cfg.Audio.Rate,
				SquelchDB:       ch.SquelchDB,
				MaxInputSamples: block,
			})
			if err != nil {
				return fmt.Errorf("group %q: %w", g.Name, err)
			}

			c := &channelRuntime{
				cfg:   ch,
				group: g.Name,
				dsp:   chain,
				hub:   stream.NewHub(hubDepth, maxAudio),
				frame: make([]byte, maxAudio),
			}
			// A silent channel should report the noise floor, not zero.
			c.levelBits.Store(math.Float64bits(dsp.LevelDB(nil)))

			runtime.channels = append(runtime.channels, c)
			r.handles = append(r.handles, Channel{
				Name: ch.Name, Group: g.Name, Freq: ch.Freq,
				AudioRate: int(r.cfg.Audio.Rate), Hub: c.hub, State: c.state,
			})
		}
		r.groups = append(r.groups, runtime)
	}
	return nil
}

func (r *Receiver) groupMatchingSource() (*groupRuntime, error) {
	center, rate := r.src.CenterFreq(), r.src.SampleRate()
	for _, g := range r.groups {
		if math.Abs(g.cfg.CenterFreq-center) <= tuningTolerance &&
			math.Abs(g.cfg.SampleRate-rate) <= tuningTolerance {
			return g, nil
		}
	}
	return nil, fmt.Errorf(
		"the source is tuned to %.4f MHz at %.3f MS/s, which matches no configured group",
		center/1e6, rate/1e6)
}

// Channels returns a handle for every channel in every group, active or not.
func (r *Receiver) Channels() []Channel { return r.handles }

// ActiveGroup names the group currently being demodulated.
func (r *Receiver) ActiveGroup() string { return r.active.Load().cfg.Name }

// SourceDescription is a one-line summary of the radio, for status output.
func (r *Receiver) SourceDescription() string { return r.src.Describe() }

// Groups describes every configured group and which one is live.
func (r *Receiver) Groups() []GroupInfo {
	active := r.ActiveGroup()

	out := make([]GroupInfo, 0, len(r.groups))
	for _, g := range r.groups {
		names := make([]string, 0, len(g.channels))
		for _, c := range g.channels {
			names = append(names, c.cfg.Name)
		}
		out = append(out, GroupInfo{
			Name: g.cfg.Name, CenterFreq: g.cfg.CenterFreq, SampleRate: g.cfg.SampleRate,
			Channels: names, Active: g.cfg.Name == active,
		})
	}
	return out
}

// Switch moves the radio to another group, blocking until the change has been
// applied or refused.
//
// Switching to the group already running is free: it does not interrupt the
// stream, so a redundant request costs no gap in the audio.
func (r *Receiver) Switch(ctx context.Context, name string) error {
	if _, ok := r.cfg.Group(name); !ok {
		return fmt.Errorf("no group named %q", name)
	}
	if name == r.ActiveGroup() {
		return nil
	}

	reply := make(chan error, 1)
	select {
	case r.switches <- &switchRequest{group: name, reply: reply}:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run receives until the context is cancelled or the source ends.
//
// Power of 10 rule 2 asks for bounded loops; this is the documented exception,
// and it terminates on context cancellation.
func (r *Receiver) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		req, err := r.runUntilInterrupted(ctx)
		if err != nil {
			return err
		}
		if req == nil {
			return nil // the context ended, or the source did
		}

		err = r.applySwitch(req.group)
		if err != nil {
			slog.Error("group switch failed; staying on the current group",
				"requested", req.group, "active", r.ActiveGroup(), "err", err)
		} else {
			slog.Info("switched group", "group", r.ActiveGroup())
		}
		req.reply <- err
	}
	return nil
}

// runUntilInterrupted streams one group until the context ends, the source
// stops, or a switch is requested. It always leaves the stream fully stopped.
func (r *Receiver) runUntilInterrupted(ctx context.Context) (*switchRequest, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	blocks, err := r.src.Start(streamCtx)
	if err != nil {
		return nil, fmt.Errorf("start source: %w", err)
	}

	req := r.pump(ctx, blocks)

	// The source must be fully stopped before anyone retunes it, so cancel and
	// then drain until the channel closes rather than merely returning.
	cancel()
	for b := range blocks {
		b.Release()
	}
	return req, nil
}

// pump feeds blocks to the active group until something interrupts it.
func (r *Receiver) pump(ctx context.Context, blocks <-chan *sdr.Block) *switchRequest {
	for {
		select {
		case <-ctx.Done():
			return nil

		case req := <-r.switches:
			return req

		case b, ok := <-blocks:
			if !ok {
				slog.Info("source stopped")
				return nil
			}
			r.process(b)
		}
	}
}

// process runs one block through every channel of the active group.
func (r *Receiver) process(b *sdr.Block) {
	if b.Overflow {
		slog.Warn("device dropped samples; the host is not keeping up")
	}

	for _, c := range r.active.Load().channels {
		audio := c.dsp.Process(b.Samples)

		c.levelBits.Store(math.Float64bits(c.dsp.LevelDB()))
		c.open.Store(c.dsp.Open())

		n := stream.EncodeULawBlock(c.frame, audio)
		c.hub.Publish(c.frame[:n])
	}
	b.Release()
}

// applySwitch retunes the radio. On failure the active group is left alone, so
// the caller's next stream restarts on the tuning that was already working.
func (r *Receiver) applySwitch(name string) error {
	var target *groupRuntime
	for _, g := range r.groups {
		if g.cfg.Name == name {
			target = g
		}
	}
	if target == nil {
		return fmt.Errorf("no group named %q", name)
	}

	if err := r.src.Retune(target.cfg.CenterFreq, target.cfg.SampleRate); err != nil {
		return fmt.Errorf("switch to group %q: %w", name, err)
	}
	r.active.Store(target)
	return nil
}
