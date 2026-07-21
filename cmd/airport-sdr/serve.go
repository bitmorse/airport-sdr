package main

import (
	"flag"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"

	"github.com/bitmorse/airport-sdr/internal/config"
	"github.com/bitmorse/airport-sdr/internal/dsp"
	"github.com/bitmorse/airport-sdr/internal/sdr"
	"github.com/bitmorse/airport-sdr/internal/stream"
	"github.com/bitmorse/airport-sdr/internal/web"
)

// hubDepth is how many audio frames a listener may fall behind before frames
// are dropped for them. At 20 ms a frame this is a fraction of a second: enough
// to ride out ordinary network jitter, little enough that nobody ends up
// listening to stale traffic.
const hubDepth = 8

// channelRuntime ties one configured channel to its DSP chain and its listeners.
type channelRuntime struct {
	cfg config.ChannelConfig
	dsp *dsp.Channel
	hub *stream.Hub

	frame []byte // reused mu-law scratch, so the audio path never allocates

	// The DSP runs on its own goroutine while HTTP handlers read status, so
	// the published state is kept in atomics rather than behind a lock. That
	// keeps the receive path free of contention with listeners.
	levelBits atomic.Uint64
	open      atomic.Bool
}

func (c *channelRuntime) state() web.ChannelState {
	return web.ChannelState{
		LevelDB:     math.Float64frombits(c.levelBits.Load()),
		SquelchOpen: c.open.Load(),
	}
}

// serveCmd runs the receiver and the web server together.
func serveCmd(cfgPath, listen string, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	iq := fs.String("iq", "", "replay this .cf32 capture instead of using the radio")
	group := fs.String("group", "", "which channel group to receive (default the first)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(cfgPath, listen)
	if err != nil {
		return err
	}

	// One group at a time until switching lands; the first is the default.
	g, err := activeGroup(cfg, *group)
	if err != nil {
		return err
	}

	src, err := openSource(cfg, g, *iq)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // shutting down anyway

	channels, err := buildChannels(cfg, g)
	if err != nil {
		return err
	}

	srv, err := web.NewServer(web.Options{
		Channels:          channelInfos(cfg, channels),
		SourceDescription: src.Describe(),
	})
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	blocks, err := src.Start(ctx)
	if err != nil {
		return fmt.Errorf("start source: %w", err)
	}
	go runDSP(blocks, channels)

	slog.Info("receiver running", "source", src.Describe(),
		"group", g.Name, "channels", len(channels))
	return srv.ListenAndServe(ctx, cfg.Server.Listen)
}

// openSource picks between the radio and a recorded capture. Replaying a
// capture makes the whole server testable, and demonstrable, with no hardware.
func openSource(cfg config.Config, g config.GroupConfig, iqFile string) (sdr.Source, error) {
	if iqFile == "" {
		return sdr.NewSoapySource(soapyOptions(cfg, g))
	}
	return sdr.NewFileSource(sdr.FileOptions{
		Path:       iqFile,
		SampleRate: g.SampleRate,
		CenterFreq: g.CenterFreq,
		BlockSize:  blockSize(g.SampleRate),
		Loop:       true,
		Realtime:   true, // pace it like a radio, so listening is realistic
	})
}

func buildChannels(cfg config.Config, g config.GroupConfig) ([]*channelRuntime, error) {
	block := blockSize(g.SampleRate)
	// Audio samples produced per input block, plus a little slack for the
	// filter boundaries.
	maxAudio := int(float64(block)*cfg.Audio.Rate/g.SampleRate) + 8

	out := make([]*channelRuntime, 0, len(g.Channels))
	for _, ch := range g.Channels {
		chain, err := dsp.NewChannel(dsp.ChannelOptions{
			Name:            ch.Name,
			Offset:          ch.Freq - g.CenterFreq,
			InputRate:       g.SampleRate,
			AudioRate:       cfg.Audio.Rate,
			SquelchDB:       ch.SquelchDB,
			MaxInputSamples: block,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, &channelRuntime{
			cfg:   ch,
			dsp:   chain,
			hub:   stream.NewHub(hubDepth, maxAudio),
			frame: make([]byte, maxAudio),
		})
	}
	return out, nil
}

func channelInfos(cfg config.Config, channels []*channelRuntime) []web.ChannelInfo {
	infos := make([]web.ChannelInfo, 0, len(channels))
	for _, c := range channels {
		infos = append(infos, web.ChannelInfo{
			Name:      c.cfg.Name,
			Freq:      c.cfg.Freq,
			AudioRate: int(cfg.Audio.Rate),
			Hub:       c.hub,
			State:     c.state,
		})
	}
	return infos
}

// runDSP is the receive loop: every block goes through every channel, and the
// resulting audio is published to whoever is listening.
//
// Publishing never blocks, so a slow listener cannot stall the radio; it only
// loses frames of its own. The loop ends when the source closes its channel.
func runDSP(blocks <-chan *sdr.Block, channels []*channelRuntime) {
	for b := range blocks {
		if b.Overflow {
			slog.Warn("device dropped samples; the host is not keeping up")
		}
		for _, c := range channels {
			audio := c.dsp.Process(b.Samples)

			c.levelBits.Store(math.Float64bits(c.dsp.LevelDB()))
			c.open.Store(c.dsp.Open())

			n := stream.EncodeULawBlock(c.frame, audio)
			c.hub.Publish(c.frame[:n])
		}
		b.Release()
	}
	slog.Info("source stopped")
}
