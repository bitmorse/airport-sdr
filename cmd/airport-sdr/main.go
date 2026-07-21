// Command airport-sdr receives AM airband voice and streams it to browsers.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/bitmorse/airport-sdr/internal/config"
)

// version is stamped at build time via -ldflags.
var version = "dev"

const defaultConfigPath = "/etc/airport-sdr/config.yaml"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("airport-sdr", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to the YAML config file")
	listen := fs.String("listen", "", "override server.listen; binding beyond loopback is deliberate")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Usage = func() { usage(fs) }

	if err := fs.Parse(args); err != nil {
		return err
	}
	setupLogging(*verbose)

	command, rest := "serve", []string(nil)
	if fs.NArg() > 0 {
		command, rest = fs.Arg(0), fs.Args()[1:]
	}

	switch command {
	case "version":
		fmt.Println(version)
		return nil
	case "validate":
		return validate(*cfgPath)
	case "serve":
		return serveCmd(*cfgPath, *listen, rest)
	case "spectrum":
		return spectrumCmd(*cfgPath, rest)
	case "probe":
		return probeCmd(*cfgPath, rest)
	case "record":
		return recordCmd(*cfgPath, rest)
	case "replay":
		return replayCmd(*cfgPath, rest)
	default:
		usage(fs)
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, "airport-sdr %s\n\nusage: airport-sdr [flags] <command>\n\n", version)
	fmt.Fprint(os.Stderr, "commands:\n"+
		"  serve      run the receiver and web server (default)\n"+
		"  validate   check the config file and exit\n"+
		"  record     capture raw IQ to a .cf32 file  [--out --duration]\n"+
		"  replay     demodulate a capture to a WAV file [--in --out --channel]\n"+
		"  version    print the version\n\nflags:\n")
	fs.PrintDefaults()
}

func setupLogging(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func loadConfig(path, listen string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	if listen != "" {
		cfg.Server.Listen = listen
		if err := cfg.Validate(); err != nil {
			return config.Config{}, fmt.Errorf("invalid --listen override: %w", err)
		}
	}
	return cfg, nil
}

func validate(path string) error {
	cfg, err := loadConfig(path, "")
	if err != nil {
		return err
	}
	fmt.Printf("%s is valid\n", path)
	fmt.Printf("  audio    %.0f Hz\n", cfg.Audio.Rate)
	fmt.Printf("  listen   %s\n", cfg.Server.Listen)

	// The radio covers one group at a time; every channel inside the active
	// group is demodulated in parallel.
	for _, g := range cfg.Groups {
		fmt.Printf("\n  group %s: %.4f MHz @ %.3f MS/s (usable +/-%.0f kHz)\n",
			g.Name, g.CenterFreq/1e6, g.SampleRate/1e6,
			g.SampleRate*config.UsableBandwidth/2/1e3)
		for _, ch := range g.Channels {
			fmt.Printf("    %-10s %.4f MHz  %s  squelch %.0f dBFS  (%+.1f kHz from centre)\n",
				ch.Name, ch.Freq/1e6, ch.Mode, ch.SquelchDB, (ch.Freq-g.CenterFreq)/1e3)
		}
	}
	return nil
}
