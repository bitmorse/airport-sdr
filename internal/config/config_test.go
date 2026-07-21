package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fieldErrors collects the Field of every ValidationError inside err, including
// those wrapped by errors.Join. Validation reports every problem at once rather
// than only the first, so tests assert on the whole set.
func fieldErrors(t *testing.T, err error) []string {
	t.Helper()
	var fields []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		var ve *ValidationError
		if errors.As(e, &ve) {
			fields = append(fields, ve.Field)
		}
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			for _, sub := range joined.Unwrap() {
				walk(sub)
			}
		}
	}
	walk(err)
	return fields
}

func assertInvalid(t *testing.T, c Config, wantField string) {
	t.Helper()
	err := c.Validate()
	if err == nil {
		t.Fatalf("expected validation to fail for %s, got nil", wantField)
	}
	for _, f := range fieldErrors(t, err) {
		if f == wantField {
			return
		}
	}
	t.Fatalf("expected a validation error on %q, got %v", wantField, fieldErrors(t, err))
}

// groundGroup is the busiest real cluster: four channels inside 170 kHz, with
// the tuner parked in the gap between the lowest two.
func groundGroup() GroupConfig {
	return GroupConfig{
		Name:       "Ground",
		CenterFreq: 121_805_000,
		SampleRate: 960_000,
		Channels: []ChannelConfig{
			{Name: "Apron S", Freq: 121_755_000, Mode: ModeAM, SquelchDB: -62},
			{Name: "Apron N", Freq: 121_855_000, Mode: ModeAM, SquelchDB: -62},
			{Name: "Ground", Freq: 121_902_000, Mode: ModeAM, SquelchDB: -62},
			{Name: "Delivery", Freq: 121_925_000, Mode: ModeAM, SquelchDB: -62},
		},
	}
}

// --- defaults ---------------------------------------------------------------

func TestDefaultConfigIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config must be valid, got: %v", err)
	}
}

func TestDefaultHasOneGroup(t *testing.T) {
	c := Default()
	if len(c.Groups) != 1 {
		t.Fatalf("default has %d groups, want 1", len(c.Groups))
	}
	g := c.Groups[0]
	if g.CenterFreq != 118_250_000 || g.SampleRate != 960_000 {
		t.Errorf("default group tuning = %.0f Hz @ %.0f S/s, want 118250000 @ 960000",
			g.CenterFreq, g.SampleRate)
	}
	if len(g.Channels) != 1 || g.Channels[0].Freq != 118_100_000 {
		t.Errorf("want a single channel on 118.1 MHz, got %+v", g.Channels)
	}
}

func TestDefaultListenIsLoopback(t *testing.T) {
	if got := Default().Server.Listen; got != "127.0.0.1:8080" {
		t.Errorf("default listen = %q, want loopback", got)
	}
}

// --- groups -----------------------------------------------------------------

func TestValidateRequiresAtLeastOneGroup(t *testing.T) {
	c := Default()
	c.Groups = nil
	assertInvalid(t, c, "groups")
}

func TestValidateGroupName(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		c := Default()
		c.Groups[0].Name = ""
		assertInvalid(t, c, "groups[0].name")
	})
	t.Run("duplicate", func(t *testing.T) {
		c := Default()
		dup := groundGroup()
		dup.Name = c.Groups[0].Name
		c.Groups = append(c.Groups, dup)
		assertInvalid(t, c, "groups[1].name")
	})
}

func TestValidateGroupRequiresChannels(t *testing.T) {
	c := Default()
	c.Groups[0].Channels = nil
	assertInvalid(t, c, "groups[0].channels")
}

func TestValidateGroupSampleRate(t *testing.T) {
	for name, rate := range map[string]float64{
		"zero":                         0,
		"negative":                     -960_000,
		"below min":                    MinSampleRate - 1,
		"above max":                    MaxSampleRate + 1,
		"not a multiple of audio rate": 1_234_567,
	} {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.Groups[0].SampleRate = rate
			assertInvalid(t, c, "groups[0].sample_rate")
		})
	}
}

func TestValidateGroupCenterFreq(t *testing.T) {
	for name, freq := range map[string]float64{
		"zero":      0,
		"negative":  -118e6,
		"below min": MinCenterFreq - 1,
		"above max": MaxCenterFreq + 1,
	} {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.Groups[0].CenterFreq = freq
			assertInvalid(t, c, "groups[0].center_freq")
		})
	}
}

// Each group tunes independently, so one group's rate must not constrain
// another's. A wide group alongside a narrow one has to be legal.
func TestValidateGroupsMayUseDifferentSampleRates(t *testing.T) {
	c := Default()
	wide := groundGroup()
	wide.SampleRate = 2_400_000
	c.Groups = append(c.Groups, wide)

	if err := c.Validate(); err != nil {
		t.Fatalf("groups with different sample rates must be valid: %v", err)
	}
}

func TestValidateRealGroupLayout(t *testing.T) {
	c := Default()
	c.Groups = []GroupConfig{groundGroup()}
	if err := c.Validate(); err != nil {
		t.Fatalf("the ground cluster layout must be valid: %v", err)
	}
}

// --- channels within a group ------------------------------------------------

// The tuner must never sit on a channel: the DC spur at the local oscillator
// measured 60 dB above the noise floor, and a channel parked there is
// unreceivable no matter what the rest of the chain does.
func TestValidateChannelMustAvoidLocalOscillator(t *testing.T) {
	for name, offset := range map[string]float64{
		"exactly on LO": 0,
		"just above":    MinLOOffset / 2,
		"just below":    -MinLOOffset / 2,
	} {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.Groups[0].Channels[0].Freq = c.Groups[0].CenterFreq + offset
			assertInvalid(t, c, "groups[0].channels[0].freq")
		})
	}
}

func TestValidateChannelMustBeInsideItsGroupsBand(t *testing.T) {
	c := Default()
	// 960 kS/s gives +/-384 kHz of usable offset; 500 kHz is outside it.
	c.Groups[0].Channels[0].Freq = c.Groups[0].CenterFreq + 500_000
	assertInvalid(t, c, "groups[0].channels[0].freq")
}

func TestValidateChannelName(t *testing.T) {
	c := Default()
	c.Groups[0].Channels[0].Name = ""
	assertInvalid(t, c, "groups[0].channels[0].name")
}

// Channel names are the keys in /ws/audio/{name}, so they must be unique across
// the whole config, not merely within a group.
func TestValidateChannelNamesAreUniqueAcrossGroups(t *testing.T) {
	c := Default()
	clash := groundGroup()
	clash.Channels[0].Name = c.Groups[0].Channels[0].Name
	c.Groups = append(c.Groups, clash)

	assertInvalid(t, c, "groups[1].channels[0].name")
}

func TestValidateChannelMode(t *testing.T) {
	c := Default()
	c.Groups[0].Channels[0].Mode = "ssb"
	assertInvalid(t, c, "groups[0].channels[0].mode")
}

func TestValidateChannelSquelch(t *testing.T) {
	for name, db := range map[string]float64{
		"too low":  MinSquelchDB - 1,
		"too high": MaxSquelchDB + 1,
	} {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.Groups[0].Channels[0].SquelchDB = db
			assertInvalid(t, c, "groups[0].channels[0].squelch_db")
		})
	}
}

// --- device and server ------------------------------------------------------

func TestValidateGain(t *testing.T) {
	t.Run("out of range when manual", func(t *testing.T) {
		c := Default()
		c.SDR.AutoGain = false
		c.SDR.Gain = MaxGain + 1
		assertInvalid(t, c, "sdr.gain")
	})
	t.Run("allows negative gain the device may support", func(t *testing.T) {
		c := Default()
		c.SDR.Gain = -12
		if err := c.Validate(); err != nil {
			t.Errorf("negative gain must be allowed: %v", err)
		}
	})
	t.Run("ignored when auto", func(t *testing.T) {
		c := Default()
		c.SDR.AutoGain = true
		c.SDR.Gain = MaxGain + 1
		if err := c.Validate(); err != nil {
			t.Errorf("gain must not be checked when auto: %v", err)
		}
	})
}

func TestValidateListenAddress(t *testing.T) {
	c := Default()
	c.Server.Listen = ""
	assertInvalid(t, c, "server.listen")
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	c := Default()
	c.Server.Listen = ""
	c.Groups[0].Name = ""
	c.Groups[0].Channels[0].Name = ""

	got := fieldErrors(t, c.Validate())
	for _, want := range []string{"server.listen", "groups[0].name", "groups[0].channels[0].name"} {
		found := false
		for _, f := range got {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing error for %q; got %v", want, got)
		}
	}
}

// --- lookup -----------------------------------------------------------------

func TestGroupLookup(t *testing.T) {
	c := Default()
	c.Groups = append(c.Groups, groundGroup())

	if g, ok := c.Group("Ground"); !ok || g.CenterFreq != 121_805_000 {
		t.Errorf("Group(\"Ground\") = %+v, %v", g, ok)
	}
	if _, ok := c.Group("Nowhere"); ok {
		t.Error("Group of an unknown name must report false")
	}
}

// --- the legacy single-group form -------------------------------------------

// Configs written before groups existed put channels at the top level and the
// tuning in sdr. They must keep working, folded into a single implicit group.
func TestLoadFoldsLegacyFlatConfigIntoOneGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "sdr:\n  center_freq: 118250000\n  sample_rate: 960000\n" +
		"channels:\n  - name: Tower\n    freq: 118100000\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Groups) != 1 {
		t.Fatalf("got %d groups, want 1 synthesised from the flat form", len(c.Groups))
	}
	g := c.Groups[0]
	if g.CenterFreq != 118_250_000 || g.SampleRate != 960_000 {
		t.Errorf("group tuning = %.0f @ %.0f, want the sdr values", g.CenterFreq, g.SampleRate)
	}
	if len(g.Channels) != 1 || g.Channels[0].Name != "Tower" {
		t.Errorf("group channels = %+v, want the top-level channel", g.Channels)
	}
}

// Both forms at once is ambiguous about which tuning applies, so it is refused
// rather than silently preferring one.
func TestLoadRejectsBothFormsTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "channels:\n  - name: Tower\n    freq: 118100000\n" +
		"groups:\n  - name: G\n    center_freq: 121805000\n    sample_rate: 960000\n" +
		"    channels:\n      - name: Ground\n        freq: 121902000\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a config using both the flat and grouped forms must be rejected")
	}
}

// --- loading ----------------------------------------------------------------

func TestLoadGroupedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "groups:\n" +
		"  - name: Tower\n    center_freq: 118250000\n    sample_rate: 960000\n" +
		"    channels:\n      - name: Tower\n        freq: 118100000\n" +
		"  - name: Ground\n    center_freq: 121805000\n    sample_rate: 960000\n" +
		"    channels:\n      - name: Ground\n        freq: 121902000\n" +
		"      - name: Delivery\n        freq: 121925000\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(c.Groups))
	}
	if got := len(c.Groups[1].Channels); got != 2 {
		t.Errorf("second group has %d channels, want 2", got)
	}
	// Per-channel defaults must still apply inside groups.
	if c.Groups[0].Channels[0].Mode != ModeAM {
		t.Errorf("mode = %q, want the default %q", c.Groups[0].Channels[0].Mode, ModeAM)
	}
	if c.Groups[0].Channels[0].SquelchDB != DefaultSquelchDB {
		t.Errorf("squelch = %v, want the default %v",
			c.Groups[0].Channels[0].SquelchDB, DefaultSquelchDB)
	}
}

func TestLoadAppliesDeviceDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "groups:\n  - name: Tower\n    center_freq: 118250000\n    sample_rate: 960000\n" +
		"    channels:\n      - name: Tower\n        freq: 118100000\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Audio.Rate != DefaultAudioRate {
		t.Errorf("audio rate = %v, want default %v", c.Audio.Rate, DefaultAudioRate)
	}
	if c.Server.Listen != Default().Server.Listen {
		t.Errorf("listen = %q, want default", c.Server.Listen)
	}
}

func TestLoadValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "groups:\n  - name: Bad\n    center_freq: 118250000\n    sample_rate: 0\n" +
		"    channels:\n      - name: Tower\n        freq: 118100000\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load must reject an invalid config")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "sdr:\n  smaple_rate: 960000\n" +
		"groups:\n  - name: Tower\n    center_freq: 118250000\n    sample_rate: 960000\n" +
		"    channels:\n      - name: Tower\n        freq: 118100000\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a typo'd key must be an error, not silently ignored")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load must fail on a missing file")
	}
}

// The shipped configuration must actually be valid; it is the first thing a
// new user runs.
func TestShippedConfigIsValid(t *testing.T) {
	c, err := Load("../../configs/config.yaml")
	if err != nil {
		t.Fatalf("configs/config.yaml must load and validate: %v", err)
	}
	if len(c.Groups) < 1 {
		t.Error("the shipped config should define at least one group")
	}
}
