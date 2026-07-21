package sdr

import (
	"strings"
	"testing"
)

// rtlCaps mimics an RTL-SDR, which reports its sample rates as a list of
// discrete points rather than a continuous range.
func rtlCaps() DeviceCaps {
	var rates []Range
	for _, r := range []float64{250_000, 960_000, 1_024_000, 2_048_000, 2_400_000} {
		rates = append(rates, Range{Min: r, Max: r})
	}
	return DeviceCaps{
		SampleRates: rates,
		Frequencies: []Range{{Min: 24_000_000, Max: 1_766_000_000}},
		Gains:       Range{Min: 0, Max: 49.6},
		Antennas:    []string{"RX"},
	}
}

// limeCaps mimics a LimeSDR, which reports continuous ranges and several
// antenna ports.
func limeCaps() DeviceCaps {
	return DeviceCaps{
		SampleRates: []Range{{Min: 100_000, Max: 61_440_000, Step: 1}},
		Frequencies: []Range{{Min: 100_000, Max: 3_800_000_000}},
		Gains:       Range{Min: 0, Max: 73},
		Antennas:    []string{"LNAH", "LNAL", "LNAW"},
	}
}

func goodRequest() TuneRequest {
	return TuneRequest{SampleRate: 960_000, CenterFreq: 118_250_000, Gain: 40}
}

// --- Range ------------------------------------------------------------------

func TestRangeContains(t *testing.T) {
	cases := map[string]struct {
		r    Range
		v    float64
		want bool
	}{
		"inside continuous":  {Range{Min: 1, Max: 10}, 5, true},
		"at min":             {Range{Min: 1, Max: 10}, 1, true},
		"at max":             {Range{Min: 1, Max: 10}, 10, true},
		"below":              {Range{Min: 1, Max: 10}, 0.5, false},
		"above":              {Range{Min: 1, Max: 10}, 10.5, false},
		"single point hit":   {Range{Min: 960_000, Max: 960_000}, 960_000, true},
		"single point miss":  {Range{Min: 960_000, Max: 960_000}, 960_001, false},
		"on step":            {Range{Min: 0, Max: 100, Step: 25}, 75, true},
		"off step":           {Range{Min: 0, Max: 100, Step: 25}, 80, false},
		"step from offset":   {Range{Min: 5, Max: 105, Step: 10}, 65, true},
		"off step w/ offset": {Range{Min: 5, Max: 105, Step: 10}, 64, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.r.Contains(c.v); got != c.want {
				t.Errorf("Range%+v.Contains(%v) = %v, want %v", c.r, c.v, got, c.want)
			}
		})
	}
}

// --- sample rate ------------------------------------------------------------

func TestResolveAcceptsSupportedSampleRate(t *testing.T) {
	got, err := Resolve(goodRequest(), rtlCaps())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.SampleRate != 960_000 {
		t.Errorf("SampleRate = %v, want 960000 unchanged", got.SampleRate)
	}
}

// A sample rate must never be silently adjusted. The decimation plan is built
// from the configured rate and requires an exact integer ratio to the audio
// rate, so snapping 960000 to a nearby 1000000 would quietly produce audio at
// the wrong speed. Refusing is the only safe answer.
func TestResolveRejectsUnsupportedSampleRate(t *testing.T) {
	req := goodRequest()
	req.SampleRate = 1_000_000

	_, err := Resolve(req, rtlCaps())
	if err == nil {
		t.Fatal("expected an unsupported sample rate to be rejected, not snapped")
	}
	if !strings.Contains(err.Error(), "1000000") {
		t.Errorf("error should name the rejected rate, got: %v", err)
	}
	if !strings.Contains(err.Error(), "960000") {
		t.Errorf("error should suggest a supported rate, got: %v", err)
	}
}

func TestResolveRejectsRateOffDeviceStep(t *testing.T) {
	req := goodRequest()
	req.SampleRate = 960_000.5

	if _, err := Resolve(req, limeCaps()); err == nil {
		t.Fatal("expected a rate off the device's step grid to be rejected")
	}
}

// --- centre frequency -------------------------------------------------------

func TestResolveAcceptsSupportedFrequency(t *testing.T) {
	got, err := Resolve(goodRequest(), rtlCaps())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.CenterFreq != 118_250_000 {
		t.Errorf("CenterFreq = %v, want unchanged", got.CenterFreq)
	}
}

func TestResolveRejectsFrequencyOutsideDeviceRange(t *testing.T) {
	req := goodRequest()
	req.CenterFreq = 10_000_000 // below an RTL-SDR's 24 MHz floor

	_, err := Resolve(req, rtlCaps())
	if err == nil {
		t.Fatal("expected a frequency below the device range to be rejected")
	}
	if !strings.Contains(err.Error(), "24") {
		t.Errorf("error should describe the supported range, got: %v", err)
	}
}

// --- gain -------------------------------------------------------------------

// Unlike sample rate, gain is continuous and a slightly different value is
// harmless, so it is clamped rather than refused. The adjustment is reported so
// it can be logged rather than silently applied.
func TestResolveClampsGainAboveDeviceMaximum(t *testing.T) {
	req := goodRequest()
	req.Gain = 90 // above the RTL-SDR's 49.6

	got, err := Resolve(req, rtlCaps())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Gain != 49.6 {
		t.Errorf("Gain = %v, want clamped to 49.6", got.Gain)
	}
	if len(got.Notes) == 0 {
		t.Error("clamping the gain must be reported in Notes")
	}
}

func TestResolveClampsGainBelowDeviceMinimum(t *testing.T) {
	req := goodRequest()
	req.Gain = -10

	got, err := Resolve(req, rtlCaps())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Gain != 0 {
		t.Errorf("Gain = %v, want clamped to 0", got.Gain)
	}
}

func TestResolveLeavesValidGainAlone(t *testing.T) {
	got, err := Resolve(goodRequest(), rtlCaps())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Gain != 40 {
		t.Errorf("Gain = %v, want 40 unchanged", got.Gain)
	}
	if len(got.Notes) != 0 {
		t.Errorf("no adjustment was needed, but Notes = %v", got.Notes)
	}
}

func TestResolveIgnoresGainWhenAutomatic(t *testing.T) {
	req := goodRequest()
	req.AutoGain = true
	req.Gain = 9999

	got, err := Resolve(req, rtlCaps())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.AutoGain {
		t.Error("AutoGain must be preserved")
	}
}

// --- antenna ----------------------------------------------------------------

func TestResolveAcceptsKnownAntenna(t *testing.T) {
	req := goodRequest()
	req.Antenna = "LNAW"

	got, err := Resolve(req, limeCaps())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Antenna != "LNAW" {
		t.Errorf("Antenna = %q, want LNAW", got.Antenna)
	}
}

func TestResolveRejectsUnknownAntenna(t *testing.T) {
	req := goodRequest()
	req.Antenna = "LNAX"

	_, err := Resolve(req, limeCaps())
	if err == nil {
		t.Fatal("expected an unknown antenna to be rejected")
	}
	// A typo'd antenna is a common mistake; the message must list the options.
	for _, want := range []string{"LNAH", "LNAL", "LNAW"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list valid antenna %q, got: %v", want, err)
		}
	}
}

func TestResolveAllowsEmptyAntenna(t *testing.T) {
	got, err := Resolve(goodRequest(), limeCaps())
	if err != nil {
		t.Fatalf("empty antenna means 'driver default' and must be accepted: %v", err)
	}
	if got.Antenna != "" {
		t.Errorf("Antenna = %q, want empty", got.Antenna)
	}
}

// --- degenerate capabilities ------------------------------------------------

// A driver that reports nothing must not cause us to sail on and hand it
// arbitrary values; that is the failure mode that takes a process down.
func TestResolveRejectsEmptyCapabilities(t *testing.T) {
	if _, err := Resolve(goodRequest(), DeviceCaps{}); err == nil {
		t.Fatal("expected an error when the device reports no capabilities")
	}
}

func TestResolveReportsAllProblemsAtOnce(t *testing.T) {
	req := TuneRequest{SampleRate: 3_000_000, CenterFreq: 10_000_000, Antenna: "NOPE"}

	_, err := Resolve(req, rtlCaps())
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"sample rate", "frequency", "antenna"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("error should mention %q; got: %v", want, err)
		}
	}
}
