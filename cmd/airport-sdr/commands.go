package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bitmorse/airport-sdr/internal/config"
	"github.com/bitmorse/airport-sdr/internal/dsp"
	"github.com/bitmorse/airport-sdr/internal/sdr"
	"github.com/bitmorse/airport-sdr/internal/stream"
)

// captureBlockDuration is how much audio one IQ block covers. 20 ms keeps
// latency low without making the per-block overhead significant.
const captureBlockDuration = 20 * time.Millisecond

func soapyOptions(cfg config.Config) sdr.SoapyOptions {
	return sdr.SoapyOptions{
		DeviceArgs: cfg.SDR.Driver,
		SampleRate: cfg.SDR.SampleRate,
		CenterFreq: cfg.SDR.CenterFreq,
		Gain:       cfg.SDR.Gain,
		AutoGain:   cfg.SDR.AutoGain,
		Antenna:    cfg.SDR.Antenna,
		PPM:        cfg.SDR.PPM,
		BlockSize:  blockSize(cfg),
	}
}

func blockSize(cfg config.Config) int {
	return int(cfg.SDR.SampleRate * captureBlockDuration.Seconds())
}

// signalContext cancels on SIGINT or SIGTERM so a capture can be stopped early
// and still close its file cleanly.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// probeCmd reports what the device says it supports, without changing it.
func probeCmd(cfgPath string, _ []string) error {
	cfg, err := loadConfig(cfgPath, "")
	if err != nil {
		return err
	}

	caps, err := sdr.ProbeDevice(cfg.SDR.Driver)
	if err != nil {
		return err
	}

	fmt.Printf("device args   %q\n", cfg.SDR.Driver)
	fmt.Printf("antennas      %s\n", strings.Join(caps.Antennas, ", "))
	fmt.Printf("gain range    %v\n", caps.Gains)
	fmt.Printf("sample rates  %s\n", describe(caps.SampleRates))
	fmt.Printf("frequencies   %s\n", describe(caps.Frequencies))

	fmt.Printf("\nconfigured: antenna %q, gain %v, %.3f MS/s at %.4f MHz\n",
		cfg.SDR.Antenna, cfg.SDR.Gain, cfg.SDR.SampleRate/1e6, cfg.SDR.CenterFreq/1e6)
	if cfg.SDR.Antenna == "" && len(caps.Antennas) > 1 {
		fmt.Println("\nno antenna is configured, so the driver picks one. on a device with\n" +
			"several ports that is a common cause of a receiver that hears nothing:\n" +
			"the default is often the high-band port, which is deaf at VHF.")
	}
	return nil
}

func describe(rs []sdr.Range) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, ", ")
}

// recordCmd captures raw IQ to a .cf32 file.
//
// This is what makes the rest of the project testable: a capture is a
// reproducible signal, so DSP changes can be compared against a known result
// rather than against whatever happened to be on the air at the time.
func recordCmd(cfgPath string, args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	out := fs.String("out", "capture.cf32", "file to write raw IQ to")
	duration := fs.Duration("duration", 60*time.Second, "how long to record")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(cfgPath, "")
	if err != nil {
		return err
	}

	src, err := sdr.NewSoapySource(soapyOptions(cfg))
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // reported by the capture itself

	f, err := os.Create(*out)
	if err != nil {
		return fmt.Errorf("create capture file: %w", err)
	}
	defer f.Close() //nolint:errcheck // flushed explicitly below

	slog.Info("recording", "device", src.Describe(), "out", *out, "duration", *duration)
	return capture(src, f, *duration, blockSize(cfg))
}

func capture(src sdr.Source, f *os.File, duration time.Duration, block int) error {
	ctx, stop := signalContext()
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	blocks, err := src.Start(ctx)
	if err != nil {
		return fmt.Errorf("start capture: %w", err)
	}

	w := bufio.NewWriterSize(f, 1<<20)
	raw := make([]byte, block*sdr.SampleBytes)

	var samples uint64
	for b := range blocks {
		n := sdr.EncodeCF32(raw, b.Samples)
		overflow := b.Overflow
		b.Release()

		if _, err := w.Write(raw[:n*sdr.SampleBytes]); err != nil {
			return fmt.Errorf("write capture: %w", err)
		}
		samples += uint64(n)
		if overflow {
			slog.Warn("device dropped samples; the host is not keeping up")
		}
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush capture: %w", err)
	}
	slog.Info("capture complete", "samples", samples, "bytes", samples*sdr.SampleBytes)
	return nil
}

// spectrumCmd prints what is actually present across the captured span.
//
// When a channel produces no audio there are two very different explanations —
// the receiver is deaf, or the channel was quiet — and the audio alone cannot
// tell them apart. Looking at the spectrum can.
func spectrumCmd(cfgPath string, args []string) error {
	fs := flag.NewFlagSet("spectrum", flag.ContinueOnError)
	in := fs.String("in", "testdata/capture.cf32", "raw IQ file to analyse")
	size := fs.Int("size", 4096, "FFT size (power of two)")
	rows := fs.Int("rows", 64, "how many frequency rows to print")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(cfgPath, "")
	if err != nil {
		return err
	}

	spec, err := dsp.NewSpectrum(*size, cfg.SDR.SampleRate)
	if err != nil {
		return err
	}
	src, err := sdr.NewFileSource(sdr.FileOptions{
		Path: *in, SampleRate: cfg.SDR.SampleRate,
		CenterFreq: cfg.SDR.CenterFreq, BlockSize: *size,
	})
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only input

	ctx, stop := signalContext()
	defer stop()
	blocks, err := src.Start(ctx)
	if err != nil {
		return err
	}

	frames := 0
	for b := range blocks {
		if len(b.Samples) == *size {
			spec.Accumulate(b.Samples)
			frames++
		}
		b.Release()
	}

	fmt.Printf("%d frames of %d bins across %.3f MS/s centred on %.4f MHz\n\n",
		frames, *size, cfg.SDR.SampleRate/1e6, cfg.SDR.CenterFreq/1e6)
	printSpectrum(spec, cfg, *rows)
	return nil
}

// printSpectrum draws the averaged and max-hold levels as a text chart.
func printSpectrum(spec *dsp.Spectrum, cfg config.Config, rows int) {
	avg, hold := spec.PowerDB(), spec.MaxHoldDB()
	perRow := spec.Bins() / rows

	// Scale the bars to the data actually present rather than a fixed range.
	lo, hi := math.Inf(1), math.Inf(-1)
	for i := range hold {
		lo, hi = math.Min(lo, avg[i]), math.Max(hi, hold[i])
	}

	fmt.Printf("%12s %8s %8s\n", "frequency", "avg dB", "peak dB")
	for r := 0; r < rows; r++ {
		start := r * perRow
		rowAvg, rowHold := math.Inf(-1), math.Inf(-1)
		for i := start; i < start+perRow; i++ {
			rowAvg, rowHold = math.Max(rowAvg, avg[i]), math.Max(rowHold, hold[i])
		}

		freq := cfg.SDR.CenterFreq + spec.BinFreq(start+perRow/2)
		fmt.Printf("%9.4f MHz %7.1f %7.1f |%s %s\n",
			freq/1e6, rowAvg, rowHold,
			bar(rowAvg, rowHold, lo, hi), annotate(freq, cfg, perRow, spec))
	}
}

// bar draws the averaged level as '#' and the max-hold excess as '-', so a
// transient transmission is visible as a tail beyond the steady level.
func bar(avg, hold, lo, hi float64) string {
	const width = 46
	scale := func(v float64) int {
		if hi <= lo {
			return 0
		}
		n := int(float64(width) * (v - lo) / (hi - lo))
		return max(0, min(width, n))
	}

	a, h := scale(avg), scale(hold)
	out := make([]byte, width)
	for i := range out {
		switch {
		case i < a:
			out[i] = '#'
		case i < h:
			out[i] = '-'
		default:
			out[i] = ' '
		}
	}
	return string(out)
}

// annotate marks the rows a reader needs to find: the configured channels and
// the local oscillator, where a DC spur is expected.
func annotate(freq float64, cfg config.Config, perRow int, spec *dsp.Spectrum) string {
	// A row covers a band, not a point. Comparing against the row centre alone
	// means a channel landing near a row boundary is never labelled, which is
	// exactly what happened the first time this ran.
	rowWidth := math.Abs(spec.BinFreq(perRow) - spec.BinFreq(0))
	covers := func(target float64) bool { return math.Abs(freq-target) <= rowWidth }

	if covers(cfg.SDR.CenterFreq) {
		return "<= LO (DC spur expected here)"
	}
	for _, ch := range cfg.Channels {
		if covers(ch.Freq) {
			return "<= " + ch.Name
		}
	}
	return ""
}

// replayCmd runs a recorded capture through the full DSP chain and writes the
// demodulated audio to a WAV file, so the result can actually be listened to.
func replayCmd(cfgPath string, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	in := fs.String("in", "capture.cf32", "raw IQ file to replay")
	out := fs.String("out", "", "WAV file to write (default <channel>.wav)")
	channel := fs.String("channel", "", "channel name to demodulate (default the first)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(cfgPath, "")
	if err != nil {
		return err
	}
	chCfg, err := pickChannel(cfg, *channel)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = chCfg.Name + ".wav"
	}

	return replayChannel(cfg, chCfg, *in, *out)
}

func pickChannel(cfg config.Config, name string) (config.ChannelConfig, error) {
	if name == "" {
		return cfg.Channels[0], nil
	}
	for _, c := range cfg.Channels {
		if c.Name == name {
			return c, nil
		}
	}
	return config.ChannelConfig{}, fmt.Errorf("no channel named %q in the config", name)
}

func replayChannel(cfg config.Config, chCfg config.ChannelConfig, in, out string) error {
	block := blockSize(cfg)

	src, err := sdr.NewFileSource(sdr.FileOptions{
		Path:       in,
		SampleRate: cfg.SDR.SampleRate,
		CenterFreq: cfg.SDR.CenterFreq,
		BlockSize:  block,
	})
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only input

	ch, err := dsp.NewChannel(dsp.ChannelOptions{
		Name:            chCfg.Name,
		Offset:          chCfg.Freq - cfg.SDR.CenterFreq,
		InputRate:       cfg.SDR.SampleRate,
		AudioRate:       cfg.Audio.Rate,
		SquelchDB:       chCfg.SquelchDB,
		MaxInputSamples: block,
	})
	if err != nil {
		return err
	}

	f, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("create wav: %w", err)
	}
	defer f.Close() //nolint:errcheck // closed after the writer below

	slog.Info("replaying", "in", in, "channel", chCfg.Name,
		"offset_khz", (chCfg.Freq-cfg.SDR.CenterFreq)/1e3, "out", out)

	return demodulateToWAV(src, ch, f, int(cfg.Audio.Rate))
}

func demodulateToWAV(src sdr.Source, ch *dsp.Channel, f *os.File, audioRate int) error {
	ctx, stop := signalContext()
	defer stop()

	blocks, err := src.Start(ctx)
	if err != nil {
		return err
	}

	w, err := stream.NewWAVWriter(f, audioRate)
	if err != nil {
		return err
	}

	var total, openBlocks, allBlocks int
	peakChannel, peakWideband := -200.0, -200.0

	for b := range blocks {
		// The wideband level covers the whole captured span, before the channel
		// filter narrows it. Comparing the two separates a receiver that hears
		// nothing at all from one that is tuned correctly onto a quiet channel.
		if lvl := dsp.LevelDB(b.Samples); lvl > peakWideband {
			peakWideband = lvl
		}

		audio := ch.Process(b.Samples)
		b.Release()

		if err := w.Write(audio); err != nil {
			return err
		}
		total += len(audio)
		allBlocks++
		if ch.Open() {
			openBlocks++
		}
		if lvl := ch.LevelDB(); lvl > peakChannel {
			peakChannel = lvl
		}
	}

	if err := w.Close(); err != nil {
		return err
	}
	reportReplay(replayStats{
		samples: total, open: openBlocks, blocks: allBlocks,
		peakChannel: peakChannel, peakWideband: peakWideband, rate: audioRate,
	})
	return nil
}

type replayStats struct {
	samples, open, blocks int
	peakChannel           float64
	peakWideband          float64
	rate                  int
}

// reportReplay prints the numbers that tell you whether a capture is worth
// listening to, and if it is not, which knob is wrong.
func reportReplay(s replayStats) {
	pct := 0.0
	if s.blocks > 0 {
		pct = 100 * float64(s.open) / float64(s.blocks)
	}
	fmt.Printf("wrote %.1f s of audio\n", float64(s.samples)/float64(s.rate))
	fmt.Printf("peak wideband level %.1f dBFS  (whole captured span)\n", s.peakWideband)
	fmt.Printf("peak channel level  %.1f dBFS  (after the channel filter)\n", s.peakChannel)
	fmt.Printf("squelch open for %.1f%% of the capture\n", pct)

	if s.open > 0 {
		return
	}
	fmt.Println("\nnothing came through the squelch.")
	switch {
	case s.peakWideband < -60:
		fmt.Println("the wideband level is at the noise floor, so the receiver is hearing\n" +
			"almost nothing. check the antenna port (sdr.antenna) and gain before\n" +
			"suspecting the channel: the wrong port for the band looks exactly like this.")
	case s.peakChannel < s.peakWideband-20:
		fmt.Printf("the band has signal (%.1f dBFS) but this channel does not (%.1f dBFS),\n"+
			"so the receiver is working and 118.1 was simply quiet. try a longer capture.\n",
			s.peakWideband, s.peakChannel)
	default:
		fmt.Printf("there is signal on the channel, so squelch_db is set too high.\n"+
			"try a threshold a few dB below %.1f.\n", s.peakChannel)
	}
}
