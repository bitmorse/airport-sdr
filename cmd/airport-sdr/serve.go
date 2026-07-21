package main

import (
	"flag"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bitmorse/airport-sdr/internal/config"
	"github.com/bitmorse/airport-sdr/internal/receiver"
	"github.com/bitmorse/airport-sdr/internal/sdr"
	"github.com/bitmorse/airport-sdr/internal/web"
)

// serveCmd runs the receiver and the web server together.
//
// This command is wiring only: the receive loop, the DSP chains and group
// switching all live in internal/receiver, where they can be tested against a
// stub radio rather than only against hardware.
func serveCmd(cfgPath, listen string, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	iq := fs.String("iq", "", "replay this .cf32 capture instead of using the radio")
	group := fs.String("group", "", "which channel group to start on (default the first)")
	embedOrigins := fs.String("embed-origin", "",
		"comma-separated origins allowed to embed the player, overriding the config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(cfgPath, listen)
	if err != nil {
		return err
	}
	if *embedOrigins != "" {
		// Same idea as --listen: a deliberate, visible override of a setting
		// whose default is deliberately closed.
		cfg.Embed.AllowedOrigins = strings.Split(*embedOrigins, ",")
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid --embed-origin: %w", err)
		}
	}
	start, err := activeGroup(cfg, *group)
	if err != nil {
		return err
	}

	src, err := openSource(cfg, start, *iq)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // shutting down anyway

	rcv, err := receiver.New(cfg, src)
	if err != nil {
		return err
	}

	srv, err := web.NewServer(web.Options{
		Channels:          channelInfos(rcv),
		SourceDescription: rcv.SourceDescription(),
		Groups:            func() []web.GroupInfo { return groupInfos(rcv) },
		Switch:            rcv.Switch,
		Embed: web.EmbedOptions{
			AllowedOrigins: cfg.Embed.AllowedOrigins,
			Width:          cfg.Embed.Width,
			Height:         cfg.Embed.Height,
		},
	})
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	go func() {
		if err := rcv.Run(ctx); err != nil {
			slog.Error("receiver stopped", "err", err)
		}
	}()

	if cfg.Embed.AllowsAnyOrigin() {
		slog.Warn("embedding is open to any site; /embed can be framed by anyone")
	} else if cfg.Embed.Enabled() {
		slog.Info("embedding enabled", "origins", cfg.Embed.AllowedOrigins)
	}

	slog.Info("receiver running", "source", rcv.SourceDescription(),
		"group", rcv.ActiveGroup(), "groups", len(cfg.Groups),
		"channels", len(rcv.Channels()))
	return srv.ListenAndServe(ctx, cfg.Server.Listen)
}

// openSource picks between the radio and a recorded capture. Replaying a
// capture makes the whole server testable, and demonstrable, with no hardware —
// though a capture holds one group's spectrum and cannot be switched away from.
func openSource(cfg config.Config, g config.GroupConfig, iqFile string) (sdr.Source, error) {
	if iqFile == "" {
		return sdr.NewSoapySource(soapyOptions(cfg, g))
	}
	return sdr.NewFileSource(sdr.FileOptions{
		Path:       iqFile,
		SampleRate: g.SampleRate,
		CenterFreq: g.CenterFreq,
		BlockSize:  sdr.BlockSizeFor(g.SampleRate),
		Loop:       true,
		Realtime:   true, // pace it like a radio, so listening is realistic
	})
}

// channelInfos adapts the receiver's channel handles for the web server, which
// deliberately knows nothing about the DSP or the radio.
func channelInfos(rcv *receiver.Receiver) []web.ChannelInfo {
	channels := rcv.Channels()

	infos := make([]web.ChannelInfo, 0, len(channels))
	for _, c := range channels {
		state := c.State
		infos = append(infos, web.ChannelInfo{
			Name:      c.Name,
			Group:     c.Group,
			Freq:      c.Freq,
			AudioRate: c.AudioRate,
			Hub:       c.Hub,
			State: func() web.ChannelState {
				s := state()
				return web.ChannelState{LevelDB: s.LevelDB, SquelchOpen: s.SquelchOpen}
			},
		})
	}
	return infos
}

// groupInfos adapts the receiver's group list for the web server.
func groupInfos(rcv *receiver.Receiver) []web.GroupInfo {
	groups := rcv.Groups()

	out := make([]web.GroupInfo, 0, len(groups))
	for _, g := range groups {
		out = append(out, web.GroupInfo{
			Name:       g.Name,
			CenterFreq: g.CenterFreq,
			SampleRate: g.SampleRate,
			Channels:   g.Channels,
			Active:     g.Active,
		})
	}
	return out
}
