// Package config defines the receiver configuration and, more importantly,
// validates it.
//
// Everything here runs before a single sample is requested from hardware. An
// SDR driver handed an out-of-range sample rate does not politely decline; it
// can take the process down with it. Validation is therefore a hard gate, not a
// convenience: every rate, frequency and gain is range-checked here, and the
// driver layer re-checks against what the device actually reports.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"

	"gopkg.in/yaml.v3"
)

// Limits applied to every configuration. Deliberately generous — they exist to
// catch nonsense (a sample rate in hertz mistaken for megahertz, a frequency
// typed with too many zeros), not to encode the capability of any one device.
// The driver layer narrows these against the hardware's reported ranges.
const (
	MinSampleRate = 48_000.0
	MaxSampleRate = 20_000_000.0

	MinCenterFreq = 100_000.0
	MaxCenterFreq = 6_000_000_000.0

	// Gain limits are deliberately wide. Some front ends express attenuation as
	// negative gain (a LimeSDR Mini reports -12 to 61 dB), so this range only
	// catches nonsense; the driver layer narrows it to what the device reports.
	MinGain = -50.0
	MaxGain = 100.0

	MinSquelchDB = -120.0
	MaxSquelchDB = 0.0

	// MinLOOffset is how far a channel must sit from the local oscillator.
	// Most front ends show a DC spur at the LO; a channel parked on it is a
	// silent quality failure, so offset tuning is enforced rather than advised.
	MinLOOffset = 12_500.0

	// UsableBandwidth is the fraction of the sample rate we trust. The outer
	// edges are where the anti-aliasing filter rolls off.
	UsableBandwidth = 0.8

	DefaultAudioRate = 8_000.0
	DefaultSquelchDB = -35.0
)

// Mode is a demodulation scheme. Airband voice is AM; NFM is reserved for later.
type Mode string

const ModeAM Mode = "am"

// ValidationError identifies the specific field that failed, so that both
// operators and tests can act on the failure rather than parse prose.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Reason)
}

func invalid(field, format string, args ...any) *ValidationError {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

type Config struct {
	SDR      SDRConfig       `yaml:"sdr"`
	Audio    AudioConfig     `yaml:"audio"`
	Server   ServerConfig    `yaml:"server"`
	Channels []ChannelConfig `yaml:"channels"`
}

type SDRConfig struct {
	// Driver is a SoapySDR device argument string, e.g. "driver=rtlsdr".
	// Leaving it empty selects the first device found.
	Driver     string  `yaml:"driver"`
	SampleRate float64 `yaml:"sample_rate"`
	CenterFreq float64 `yaml:"center_freq"`
	Gain       float64 `yaml:"gain"`
	AutoGain   bool    `yaml:"auto_gain"`
	Antenna    string  `yaml:"antenna"`
	PPM        float64 `yaml:"ppm"`
}

type AudioConfig struct {
	Rate float64 `yaml:"rate"`
}

type ServerConfig struct {
	// Listen defaults to loopback. Exposing the receiver beyond this host must
	// be a deliberate act, so a public bind address is never a default.
	Listen string `yaml:"listen"`
}

type ChannelConfig struct {
	Name      string  `yaml:"name"`
	Freq      float64 `yaml:"freq"`
	Mode      Mode    `yaml:"mode"`
	SquelchDB float64 `yaml:"squelch_db"`
}

// DefaultChannel supplies the per-channel defaults applied to any channel in a
// config file that omits them.
func DefaultChannel() ChannelConfig {
	return ChannelConfig{Mode: ModeAM, SquelchDB: DefaultSquelchDB}
}

// UnmarshalYAML fills unset fields from DefaultChannel. Doing it here rather
// than with sentinel zero values means squelch_db: 0 stays a real (if useless)
// setting instead of being silently reinterpreted as "unset".
func (c *ChannelConfig) UnmarshalYAML(node *yaml.Node) error {
	// A distinct type breaks the recursion back into this method.
	type plain ChannelConfig
	decoded := plain(DefaultChannel())
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = ChannelConfig(decoded)
	return nil
}

// Default is the edge profile: 960 kS/s (a native RTL-SDR rate, and gentle on a
// Pi's USB bus) centred at 118.25 MHz, which places tower on 118.1 and ground on
// 118.4 inside one capture with neither sitting on the local oscillator.
func Default() Config {
	tower := DefaultChannel()
	tower.Name = "Tower"
	tower.Freq = 118_100_000

	return Config{
		SDR: SDRConfig{
			SampleRate: 960_000,
			CenterFreq: 118_250_000,
			Gain:       40,
		},
		Audio:    AudioConfig{Rate: DefaultAudioRate},
		Server:   ServerConfig{Listen: "127.0.0.1:8080"},
		Channels: []ChannelConfig{tower},
	}
}

// Validate reports every problem it finds rather than stopping at the first, so
// that fixing a config file is one edit rather than several rounds.
func (c Config) Validate() error {
	errs := c.SDR.validate(c.Audio.Rate)
	errs = append(errs, c.Audio.validate()...)
	errs = append(errs, c.Server.validate()...)
	errs = append(errs, c.validateChannels()...)
	return errors.Join(errs...)
}

func (s SDRConfig) validate(audioRate float64) []error {
	var errs []error

	switch {
	case math.IsNaN(s.SampleRate) || s.SampleRate <= 0:
		errs = append(errs, invalid("sdr.sample_rate", "must be positive, got %v", s.SampleRate))
	case s.SampleRate < MinSampleRate || s.SampleRate > MaxSampleRate:
		errs = append(errs, invalid("sdr.sample_rate",
			"%v is outside [%v, %v]", s.SampleRate, MinSampleRate, MaxSampleRate))
	case audioRate > 0 && math.Mod(s.SampleRate, audioRate) != 0:
		// The DSP chain decimates by an integer factor throughout. Catching a
		// non-integer ratio here gives a clear message instead of an obscure
		// failure inside the decimation planner.
		errs = append(errs, invalid("sdr.sample_rate",
			"must be an exact multiple of the audio rate %v, got %v", audioRate, s.SampleRate))
	}

	if math.IsNaN(s.CenterFreq) || s.CenterFreq < MinCenterFreq || s.CenterFreq > MaxCenterFreq {
		errs = append(errs, invalid("sdr.center_freq",
			"%v is outside [%v, %v]", s.CenterFreq, MinCenterFreq, MaxCenterFreq))
	}
	if !s.AutoGain && (math.IsNaN(s.Gain) || s.Gain < MinGain || s.Gain > MaxGain) {
		errs = append(errs, invalid("sdr.gain",
			"%v is outside [%v, %v]; set auto_gain to let the driver decide",
			s.Gain, MinGain, MaxGain))
	}
	if math.Abs(s.PPM) > 200 {
		errs = append(errs, invalid("sdr.ppm", "%v is implausible", s.PPM))
	}
	return errs
}

func (a AudioConfig) validate() []error {
	if a.Rate <= 0 {
		return []error{invalid("audio.rate", "must be positive, got %v", a.Rate)}
	}
	return nil
}

func (s ServerConfig) validate() []error {
	if s.Listen == "" {
		return []error{invalid("server.listen", "must be set, e.g. 127.0.0.1:8080")}
	}
	return nil
}

func (c Config) validateChannels() []error {
	if len(c.Channels) == 0 {
		return []error{invalid("channels", "at least one channel is required")}
	}

	var errs []error
	seen := make(map[string]int, len(c.Channels))
	for i, ch := range c.Channels {
		errs = append(errs, ch.validate(i, c.SDR)...)
		if prev, dup := seen[ch.Name]; dup && ch.Name != "" {
			errs = append(errs, invalid(fmt.Sprintf("channels[%d].name", i),
				"duplicates channels[%d]", prev))
		}
		seen[ch.Name] = i
	}
	return errs
}

func (ch ChannelConfig) validate(i int, sdr SDRConfig) []error {
	var errs []error
	field := func(name string) string { return fmt.Sprintf("channels[%d].%s", i, name) }

	if ch.Name == "" {
		errs = append(errs, invalid(field("name"), "must not be empty"))
	}
	if ch.Mode != ModeAM {
		errs = append(errs, invalid(field("mode"), "unsupported mode %q, want %q", ch.Mode, ModeAM))
	}
	if math.IsNaN(ch.SquelchDB) || ch.SquelchDB < MinSquelchDB || ch.SquelchDB > MaxSquelchDB {
		errs = append(errs, invalid(field("squelch_db"),
			"%v is outside [%v, %v]", ch.SquelchDB, MinSquelchDB, MaxSquelchDB))
	}
	errs = append(errs, ch.validatePlacement(field("freq"), sdr)...)
	return errs
}

// validatePlacement checks the channel against the captured band: inside the
// usable bandwidth, and clear of the local oscillator.
func (ch ChannelConfig) validatePlacement(field string, sdr SDRConfig) []error {
	if sdr.SampleRate <= 0 || math.IsNaN(ch.Freq) {
		// The sample rate is already reported as invalid; a second complaint
		// about a band we cannot compute would only be noise.
		return nil
	}

	offset := ch.Freq - sdr.CenterFreq
	usable := sdr.SampleRate * UsableBandwidth / 2

	if math.Abs(offset) > usable {
		return []error{invalid(field,
			"%.6f MHz is %.1f kHz from centre, outside the usable ±%.1f kHz at %v S/s",
			ch.Freq/1e6, offset/1e3, usable/1e3, sdr.SampleRate)}
	}
	if math.Abs(offset) < MinLOOffset {
		return []error{invalid(field,
			"%.6f MHz is only %.1f kHz from the local oscillator; keep it at least %.1f kHz away "+
				"to avoid the DC spur (move sdr.center_freq)",
			ch.Freq/1e6, math.Abs(offset)/1e3, MinLOOffset/1e3)}
	}
	return nil
}

// Load reads a YAML config, applies defaults for anything omitted, and
// validates the result. Unknown keys are an error: a typo'd setting that is
// silently ignored is worse than one that fails loudly.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file, nothing to report

	cfg := Default()
	cfg.Channels = nil // a channel list in the file replaces the default, never merges

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}
