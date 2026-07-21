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

func TestDefaultConfigIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config must be valid, got: %v", err)
	}
}

// The edge default must be the documented one: 960 kS/s centred at 118.25 MHz
// so that tower (118.1) and ground (118.4) both sit inside one capture without
// either landing on the local oscillator.
func TestDefaultConfigMatchesEdgeProfile(t *testing.T) {
	c := Default()
	if c.SDR.SampleRate != 960_000 {
		t.Errorf("sample rate = %v, want 960000", c.SDR.SampleRate)
	}
	if c.SDR.CenterFreq != 118_250_000 {
		t.Errorf("centre freq = %v, want 118250000", c.SDR.CenterFreq)
	}
	if len(c.Channels) != 1 || c.Channels[0].Freq != 118_100_000 {
		t.Errorf("want exactly one channel on 118.1 MHz, got %+v", c.Channels)
	}
}

func TestValidateSampleRate(t *testing.T) {
	for name, rate := range map[string]float64{
		"zero":      0,
		"negative":  -960_000,
		"below min": MinSampleRate - 1,
		"above max": MaxSampleRate + 1,
	} {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.SDR.SampleRate = rate
			// Keep the channel inside whatever band this rate implies so the
			// sample rate is the only thing under test.
			c.Channels[0].Freq = c.SDR.CenterFreq - 150_000
			assertInvalid(t, c, "sdr.sample_rate")
		})
	}
}

func TestValidateCenterFreq(t *testing.T) {
	for name, freq := range map[string]float64{
		"zero":      0,
		"negative":  -118e6,
		"below min": MinCenterFreq - 1,
		"above max": MaxCenterFreq + 1,
	} {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.SDR.CenterFreq = freq
			assertInvalid(t, c, "sdr.center_freq")
		})
	}
}

// Integer decimation is a hard requirement of the DSP chain, so a sample rate
// that is not an exact multiple of the audio rate must be rejected in config
// rather than surfacing later as a confusing planner failure.
func TestValidateRejectsNonIntegerDecimation(t *testing.T) {
	c := Default()
	c.SDR.SampleRate = 1_234_567
	c.Channels[0].Freq = c.SDR.CenterFreq - 150_000
	assertInvalid(t, c, "sdr.sample_rate")
}

func TestValidateAcceptsIntegerDecimation(t *testing.T) {
	for _, rate := range []float64{960_000, 1_024_000, 2_400_000, 2_048_000} {
		c := Default()
		c.SDR.SampleRate = rate
		if err := c.Validate(); err != nil {
			t.Errorf("sample rate %v should be valid: %v", rate, err)
		}
	}
}

func TestValidateChannelMustBeInsideCapturedBand(t *testing.T) {
	c := Default()
	// 960 kS/s gives ±384 kHz of usable offset; 500 kHz is outside it.
	c.Channels[0].Freq = c.SDR.CenterFreq + 500_000
	assertInvalid(t, c, "channels[0].freq")
}

// Most SDR front ends show a DC spur at the local oscillator. A channel sitting
// on it is a silent quality failure, so offset tuning is enforced, not advised.
func TestValidateChannelMustAvoidLocalOscillator(t *testing.T) {
	for name, offset := range map[string]float64{
		"exactly on LO": 0,
		"just above":    MinLOOffset / 2,
		"just below":    -MinLOOffset / 2,
	} {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.Channels[0].Freq = c.SDR.CenterFreq + offset
			assertInvalid(t, c, "channels[0].freq")
		})
	}
}

func TestValidateChannelNames(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		c := Default()
		c.Channels[0].Name = ""
		assertInvalid(t, c, "channels[0].name")
	})
	t.Run("duplicate", func(t *testing.T) {
		c := Default()
		c.Channels = append(c.Channels, ChannelConfig{
			Name: c.Channels[0].Name, Freq: 118_400_000, Mode: ModeAM, SquelchDB: -35,
		})
		assertInvalid(t, c, "channels[1].name")
	})
}

func TestValidateRequiresAtLeastOneChannel(t *testing.T) {
	c := Default()
	c.Channels = nil
	assertInvalid(t, c, "channels")
}

func TestValidateSquelch(t *testing.T) {
	for name, db := range map[string]float64{
		"too low":  MinSquelchDB - 1,
		"too high": MaxSquelchDB + 1,
	} {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.Channels[0].SquelchDB = db
			assertInvalid(t, c, "channels[0].squelch_db")
		})
	}
}

func TestValidateMode(t *testing.T) {
	c := Default()
	c.Channels[0].Mode = "ssb"
	assertInvalid(t, c, "channels[0].mode")
}

func TestValidateGain(t *testing.T) {
	t.Run("out of range when manual", func(t *testing.T) {
		c := Default()
		c.SDR.AutoGain = false
		c.SDR.Gain = MaxGain + 1
		assertInvalid(t, c, "sdr.gain")
	})
	// Some front ends offer attenuation as negative gain: a LimeSDR Mini
	// reports a range of -12 to 61 dB. Refusing negatives here would make a
	// setting the hardware supports unconfigurable.
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

// Exposure must always be a deliberate act, so the default binds to loopback.
func TestDefaultListenIsLoopback(t *testing.T) {
	if got := Default().Server.Listen; got != "127.0.0.1:8080" {
		t.Errorf("default listen = %q, want loopback", got)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	c := Default()
	c.SDR.SampleRate = 0
	c.Server.Listen = ""
	c.Channels[0].Name = ""

	got := fieldErrors(t, c.Validate())
	for _, want := range []string{"sdr.sample_rate", "server.listen", "channels[0].name"} {
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

func TestLoadAppliesDefaultsForOmittedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "channels:\n  - name: Tower\n    freq: 118100000\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.SDR.SampleRate != Default().SDR.SampleRate {
		t.Errorf("sample rate = %v, want default %v", c.SDR.SampleRate, Default().SDR.SampleRate)
	}
	if c.Channels[0].Mode != ModeAM {
		t.Errorf("mode = %q, want default %q", c.Channels[0].Mode, ModeAM)
	}
}

func TestLoadValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "sdr:\n  sample_rate: 0\nchannels:\n  - name: Tower\n    freq: 118100000\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load must reject an invalid config")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "sdr:\n  smaple_rate: 960000\nchannels:\n  - name: Tower\n    freq: 118100000\n"
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
