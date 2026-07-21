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
	"net/url"
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
	// Most front ends show a DC spur at the LO — measured here at 60 dB above
	// the noise floor — and a channel parked on it is unreceivable, so offset
	// tuning is enforced rather than advised.
	MinLOOffset = 12_500.0

	// UsableBandwidth is the fraction of the sample rate we trust. The outer
	// edges are where the anti-aliasing filter rolls off.
	UsableBandwidth = 0.8

	// DefaultMaxListeners bounds concurrent listeners. Each one costs buffers
	// and a goroutine, so an unbounded count is a memory-growth path reachable
	// by anyone who can open a connection.
	DefaultMaxListeners = 50

	DefaultAudioRate = 8_000.0
	DefaultSquelchDB = -35.0

	// legacyGroupName is given to the single group synthesised from a config
	// written before groups existed.
	legacyGroupName = "Default"

	// Default size of the embeddable player. It is a single button, so it is
	// small; an embedder can override the iframe dimensions anyway.
	DefaultEmbedWidth  = 230
	DefaultEmbedHeight = 48

	// EmbedAnyOrigin permits any site to frame the player.
	EmbedAnyOrigin = "*"
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
	SDR    SDRConfig     `yaml:"sdr"`
	Audio  AudioConfig   `yaml:"audio"`
	Server ServerConfig  `yaml:"server"`
	Embed  EmbedConfig   `yaml:"embed"`
	Groups []GroupConfig `yaml:"groups"`

	// Channels is the pre-groups form: a flat channel list tuned by
	// sdr.center_freq and sdr.sample_rate. Load folds it into a single group,
	// so nothing downstream ever sees it.
	Channels []ChannelConfig `yaml:"channels"`
}

type SDRConfig struct {
	// Driver is a SoapySDR device argument string, e.g. "driver=rtlsdr".
	// Empty selects the first device found, but naming the driver also stops
	// SoapySDR probing every installed module.
	Driver   string  `yaml:"driver"`
	Gain     float64 `yaml:"gain"`
	AutoGain bool    `yaml:"auto_gain"`
	Antenna  string  `yaml:"antenna"`
	PPM      float64 `yaml:"ppm"`

	// CenterFreq and SampleRate belong to a group. They remain here only to
	// support the pre-groups config form.
	CenterFreq float64 `yaml:"center_freq"`
	SampleRate float64 `yaml:"sample_rate"`
}

// EmbedConfig controls whether other sites may frame a channel player.
//
// Embedding is off unless an origin is listed. Framing the receiver lets
// another site put its visitors on your radio, so it is opt-in per origin
// rather than something a default turns on.
type EmbedConfig struct {
	// AllowedOrigins are the sites permitted to frame the player, each a bare
	// origin such as "https://example.com". The single entry "*" permits any
	// site, which suits a deliberately public receiver and nothing else.
	AllowedOrigins []string `yaml:"allowed_origins"`
	// Width and Height are the default iframe dimensions advertised by oEmbed.
	Width  int `yaml:"width"`
	Height int `yaml:"height"`
}

// Enabled reports whether any site may embed the player.
func (e EmbedConfig) Enabled() bool { return len(e.AllowedOrigins) > 0 }

// AllowsAnyOrigin reports whether the allowlist is the wildcard.
func (e EmbedConfig) AllowsAnyOrigin() bool {
	return len(e.AllowedOrigins) == 1 && e.AllowedOrigins[0] == EmbedAnyOrigin
}

func (e EmbedConfig) validate() []error {
	var errs []error

	for i, origin := range e.AllowedOrigins {
		if origin == EmbedAnyOrigin {
			if len(e.AllowedOrigins) > 1 {
				errs = append(errs, invalid("embed.allowed_origins",
					"%q permits every site, so listing it alongside specific origins is "+
						"contradictory; use either the wildcard or an allowlist", EmbedAnyOrigin))
			}
			continue
		}
		if err := validateOrigin(origin); err != nil {
			errs = append(errs, invalid(
				fmt.Sprintf("embed.allowed_origins[%d]", i), "%s", err.Error()))
		}
	}

	if e.Width < 0 {
		errs = append(errs, invalid("embed.width", "must not be negative, got %d", e.Width))
	}
	if e.Height < 0 {
		errs = append(errs, invalid("embed.height", "must not be negative, got %d", e.Height))
	}
	return errs
}

// validateOrigin checks that s is a bare origin: scheme, host and optional
// port, and nothing else. Pasting a whole page URL is the usual mistake, and it
// would never match the Origin header a browser actually sends.
func validateOrigin(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}

	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %v", s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q needs an http:// or https:// scheme", s)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", s)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf(
			"%q is a URL, not an origin; use just the scheme, host and port, as in %q",
			s, u.Scheme+"://"+u.Host)
	}
	return nil
}

type AudioConfig struct {
	Rate float64 `yaml:"rate"`
}

type ServerConfig struct {
	// Listen defaults to loopback. Exposing the receiver beyond this host must
	// be a deliberate act, so a public bind address is never a default.
	Listen string `yaml:"listen"`
	// MaxListeners caps concurrent audio connections across all channels.
	// Zero means unlimited, which is only sensible on a trusted network.
	MaxListeners int `yaml:"max_listeners"`
}

// GroupConfig is one tuner position and the channels it covers.
//
// A single radio can only listen to one slice of spectrum at a time, but every
// channel inside that slice is demodulated in parallel. Grouping channels by
// the tuning that covers them is what lets a whole airfield's frequencies be
// configured, with the receiver switching between groups on request.
type GroupConfig struct {
	Name string `yaml:"name"`
	// CenterFreq must sit clear of every channel in the group; see MinLOOffset.
	CenterFreq float64         `yaml:"center_freq"`
	SampleRate float64         `yaml:"sample_rate"`
	Channels   []ChannelConfig `yaml:"channels"`
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
// Pi's USB bus) centred at 118.25 MHz, which places tower on 118.1 clear of the
// local oscillator.
func Default() Config {
	tower := DefaultChannel()
	tower.Name = "Tower"
	tower.Freq = 118_100_000

	return Config{
		SDR: SDRConfig{
			Gain: 61,
			// Retained for the pre-groups config form.
			CenterFreq: 118_250_000,
			SampleRate: 960_000,
		},
		Audio:  AudioConfig{Rate: DefaultAudioRate},
		Server: ServerConfig{Listen: "127.0.0.1:8080", MaxListeners: DefaultMaxListeners},
		// No origins: embedding is off until the operator opts in.
		Embed: EmbedConfig{Width: DefaultEmbedWidth, Height: DefaultEmbedHeight},
		Groups: []GroupConfig{{
			Name:       "Tower",
			CenterFreq: 118_250_000,
			SampleRate: 960_000,
			Channels:   []ChannelConfig{tower},
		}},
	}
}

// Group returns the named group.
func (c Config) Group(name string) (GroupConfig, bool) {
	for _, g := range c.Groups {
		if g.Name == name {
			return g, true
		}
	}
	return GroupConfig{}, false
}

// normalise folds the pre-groups form into Groups so that everything
// downstream deals only with groups.
func (c *Config) normalise() error {
	if c.Embed.Width == 0 {
		c.Embed.Width = DefaultEmbedWidth
	}
	if c.Embed.Height == 0 {
		c.Embed.Height = DefaultEmbedHeight
	}

	switch {
	case len(c.Groups) > 0 && len(c.Channels) > 0:
		return errors.New(
			"config uses both the grouped form and the older flat `channels` list; " +
				"which tuning applies is ambiguous, so move the channels into a group")

	case len(c.Channels) > 0:
		c.Groups = []GroupConfig{{
			Name:       legacyGroupName,
			CenterFreq: c.SDR.CenterFreq,
			SampleRate: c.SDR.SampleRate,
			Channels:   c.Channels,
		}}
		c.Channels = nil
	}
	return nil
}

// Validate reports every problem it finds rather than stopping at the first, so
// that fixing a config file is one edit rather than several rounds.
func (c Config) Validate() error {
	errs := c.SDR.validate()
	errs = append(errs, c.Audio.validate()...)
	errs = append(errs, c.Server.validate()...)
	errs = append(errs, c.Embed.validate()...)
	errs = append(errs, c.validateGroups()...)
	return errors.Join(errs...)
}

func (s SDRConfig) validate() []error {
	var errs []error

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
	var errs []error
	if s.Listen == "" {
		errs = append(errs, invalid("server.listen", "must be set, e.g. 127.0.0.1:8080"))
	}
	if s.MaxListeners < 0 {
		errs = append(errs, invalid("server.max_listeners",
			"must not be negative, got %d; use 0 for unlimited", s.MaxListeners))
	}
	return errs
}

// validateGroups checks every group, and the uniqueness of names across them.
func (c Config) validateGroups() []error {
	if len(c.Groups) == 0 {
		return []error{invalid("groups", "at least one group is required")}
	}

	var errs []error
	groupNames := make(map[string]int, len(c.Groups))
	// Channel names key the streaming endpoints, so they must be unique across
	// the whole config rather than merely within a group.
	channelNames := make(map[string]string)

	for i, g := range c.Groups {
		field := fmt.Sprintf("groups[%d].name", i)
		if prev, dup := groupNames[g.Name]; dup && g.Name != "" {
			errs = append(errs, invalid(field, "duplicates groups[%d]", prev))
		}
		groupNames[g.Name] = i

		errs = append(errs, g.validate(i, c.Audio.Rate, channelNames)...)
	}
	return errs
}

// validate checks one group and records its channel names in seen.
func (g GroupConfig) validate(i int, audioRate float64, seen map[string]string) []error {
	field := func(name string) string { return fmt.Sprintf("groups[%d].%s", i, name) }

	var errs []error
	if g.Name == "" {
		errs = append(errs, invalid(field("name"), "must not be empty"))
	}
	errs = append(errs, g.validateTuning(field, audioRate)...)

	if len(g.Channels) == 0 {
		errs = append(errs, invalid(field("channels"), "a group needs at least one channel"))
		return errs
	}

	for j, ch := range g.Channels {
		errs = append(errs, ch.validate(i, j, g)...)

		if ch.Name == "" {
			continue
		}
		if other, dup := seen[ch.Name]; dup {
			errs = append(errs, invalid(
				fmt.Sprintf("groups[%d].channels[%d].name", i, j),
				"duplicates the channel of the same name in %s; channel names key the "+
					"streaming endpoints and must be unique across all groups", other))
		}
		seen[ch.Name] = g.Name
	}
	return errs
}

// validateTuning checks the group's own centre frequency and sample rate.
func (g GroupConfig) validateTuning(field func(string) string, audioRate float64) []error {
	var errs []error

	switch {
	case math.IsNaN(g.SampleRate) || g.SampleRate <= 0:
		errs = append(errs, invalid(field("sample_rate"),
			"must be positive, got %v", g.SampleRate))
	case g.SampleRate < MinSampleRate || g.SampleRate > MaxSampleRate:
		errs = append(errs, invalid(field("sample_rate"),
			"%v is outside [%v, %v]", g.SampleRate, MinSampleRate, MaxSampleRate))
	case audioRate > 0 && math.Mod(g.SampleRate, audioRate) != 0:
		// The DSP chain decimates by an integer factor throughout. Catching a
		// non-integer ratio here gives a clear message instead of an obscure
		// failure inside the decimation planner.
		errs = append(errs, invalid(field("sample_rate"),
			"must be an exact multiple of the audio rate %v, got %v", audioRate, g.SampleRate))
	}

	if math.IsNaN(g.CenterFreq) || g.CenterFreq < MinCenterFreq || g.CenterFreq > MaxCenterFreq {
		errs = append(errs, invalid(field("center_freq"),
			"%v is outside [%v, %v]", g.CenterFreq, MinCenterFreq, MaxCenterFreq))
	}
	return errs
}

func (ch ChannelConfig) validate(groupIdx, chIdx int, g GroupConfig) []error {
	var errs []error
	field := func(name string) string {
		return fmt.Sprintf("groups[%d].channels[%d].%s", groupIdx, chIdx, name)
	}

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
	errs = append(errs, ch.validatePlacement(field("freq"), g)...)
	return errs
}

// validatePlacement checks the channel against its group's captured band:
// inside the usable bandwidth, and clear of the local oscillator.
func (ch ChannelConfig) validatePlacement(field string, g GroupConfig) []error {
	if g.SampleRate <= 0 || math.IsNaN(ch.Freq) {
		// The sample rate is already reported as invalid; a second complaint
		// about a band we cannot compute would only be noise.
		return nil
	}

	offset := ch.Freq - g.CenterFreq
	usable := g.SampleRate * UsableBandwidth / 2

	if math.Abs(offset) > usable {
		return []error{invalid(field,
			"%.6f MHz is %.1f kHz from the centre of group %q, outside the usable "+
				"+/-%.1f kHz at %v S/s",
			ch.Freq/1e6, offset/1e3, g.Name, usable/1e3, g.SampleRate)}
	}
	if math.Abs(offset) < MinLOOffset {
		return []error{invalid(field,
			"%.6f MHz is only %.1f kHz from the local oscillator of group %q; keep it at "+
				"least %.1f kHz away to avoid the DC spur (move the group's center_freq)",
			ch.Freq/1e6, math.Abs(offset)/1e3, g.Name, MinLOOffset/1e3)}
	}
	return nil
}

// Load reads a YAML config, applies defaults for anything omitted, folds the
// older flat form into a group, and validates the result. Unknown keys are an
// error: a typo'd setting that is silently ignored is worse than one that fails
// loudly.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file, nothing to report

	cfg := Default()
	// Lists in the file replace the defaults rather than merging with them.
	cfg.Groups, cfg.Channels = nil, nil

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.normalise(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}
