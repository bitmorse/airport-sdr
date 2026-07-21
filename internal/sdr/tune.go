package sdr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// hzTolerance is the slack allowed when matching a requested value against a
// device's reported range. Drivers report these as float64 hertz, so a
// millihertz of rounding must not be mistaken for an unsupported setting.
const hzTolerance = 1e-3

// Range is an inclusive range of values a device reports as supported. A Step
// of zero means the range is continuous; otherwise only multiples of Step above
// Min are valid.
type Range struct {
	Min  float64
	Max  float64
	Step float64
}

// Contains reports whether v is a value the device actually accepts.
func (r Range) Contains(v float64) bool {
	if v < r.Min-hzTolerance || v > r.Max+hzTolerance {
		return false
	}
	if r.Step <= 0 {
		return true
	}
	steps := math.Round((v - r.Min) / r.Step)
	return math.Abs(r.Min+steps*r.Step-v) <= hzTolerance
}

func (r Range) String() string {
	if r.Min == r.Max {
		return formatHz(r.Min)
	}
	if r.Step > 0 {
		return fmt.Sprintf("%s-%s step %s", formatHz(r.Min), formatHz(r.Max), formatHz(r.Step))
	}
	return fmt.Sprintf("%s-%s", formatHz(r.Min), formatHz(r.Max))
}

// formatHz prints without an exponent, so error messages show 24000000 rather
// than 2.4e+07 and can be compared against a config file by eye.
func formatHz(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// DeviceCaps is what a driver reports about itself.
type DeviceCaps struct {
	SampleRates []Range
	Frequencies []Range
	Gains       Range
	Antennas    []string
}

// TuneRequest is the configuration we would like to apply.
type TuneRequest struct {
	SampleRate float64
	CenterFreq float64
	Gain       float64
	AutoGain   bool
	Antenna    string
}

// Resolution is a validated request, ready to hand to a driver.
type Resolution struct {
	SampleRate float64
	CenterFreq float64
	Gain       float64
	AutoGain   bool
	Antenna    string
	// Notes describes any adjustment made, for logging. An empty Notes means
	// the request was applied exactly as asked.
	Notes []string
}

// Resolve checks a tune request against what the device says it supports.
//
// This is the layer that stands between a config file and a driver. Some SDR
// drivers respond to an out-of-range value by failing in ways that take the
// process with them, so nothing reaches the hardware until it has been checked
// against the device's own reported limits.
//
// The two kinds of setting are treated differently on purpose:
//
//   - Sample rate and frequency are refused if unsupported, never adjusted.
//     The decimation plan is derived from the configured sample rate and needs
//     an exact integer ratio to the audio rate, so quietly moving 960000 to a
//     nearby supported rate would produce audio at subtly the wrong speed —
//     the kind of fault that is very hard to diagnose by ear.
//   - Gain is continuous and a slightly different value is harmless, so it is
//     clamped and the adjustment recorded in Notes.
func Resolve(req TuneRequest, caps DeviceCaps) (Resolution, error) {
	res := Resolution{
		SampleRate: req.SampleRate,
		CenterFreq: req.CenterFreq,
		Gain:       req.Gain,
		AutoGain:   req.AutoGain,
		Antenna:    req.Antenna,
	}

	var errs []error
	errs = append(errs, checkSampleRate(req.SampleRate, caps.SampleRates))
	errs = append(errs, checkFrequency(req.CenterFreq, caps.Frequencies))
	errs = append(errs, checkAntenna(req.Antenna, caps.Antennas))

	if !req.AutoGain {
		gain, note := clampGain(req.Gain, caps.Gains)
		res.Gain = gain
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Resolution{}, err
	}
	return res, nil
}

func checkSampleRate(rate float64, supported []Range) error {
	if len(supported) == 0 {
		return errors.New("device reports no supported sample rates")
	}
	if containsAny(supported, rate) {
		return nil
	}
	return fmt.Errorf("sample rate %s is not supported by this device (supported: %s)",
		formatHz(rate), describeRanges(supported))
}

func checkFrequency(freq float64, supported []Range) error {
	if len(supported) == 0 {
		return errors.New("device reports no supported frequency ranges")
	}
	if containsAny(supported, freq) {
		return nil
	}
	return fmt.Errorf("centre frequency %s is outside this device's tuning range (%s)",
		formatHz(freq), describeRanges(supported))
}

func checkAntenna(name string, available []string) error {
	if name == "" {
		return nil // let the driver pick its default
	}
	if len(available) == 0 {
		return fmt.Errorf("antenna %q requested, but the device reports no antenna ports", name)
	}
	for _, a := range available {
		if a == name {
			return nil
		}
	}
	return fmt.Errorf("antenna %q is not available on this device (offers: %s)",
		name, strings.Join(available, ", "))
}

// clampGain limits gain to the device's range, returning a note when it had to
// move the value.
func clampGain(gain float64, r Range) (float64, string) {
	if r.Max <= r.Min {
		return gain, "" // device did not report a usable gain range
	}
	switch {
	case gain > r.Max:
		return r.Max, fmt.Sprintf("gain %g clamped to the device maximum %g", gain, r.Max)
	case gain < r.Min:
		return r.Min, fmt.Sprintf("gain %g raised to the device minimum %g", gain, r.Min)
	default:
		return gain, ""
	}
}

func containsAny(ranges []Range, v float64) bool {
	for _, r := range ranges {
		if r.Contains(v) {
			return true
		}
	}
	return false
}

func describeRanges(ranges []Range) string {
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, ", ")
}
