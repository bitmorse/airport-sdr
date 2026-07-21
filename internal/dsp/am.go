package dsp

import (
	"math"

	"github.com/bitmorse/airport-sdr/internal/assert"
)

// levelFloorDB is reported for a silent block. A finite floor keeps every
// downstream comparison well defined; negative infinity would not.
const levelFloorDB = -200.0

// LevelDB returns the RMS level of a baseband block in dBFS.
//
// For AM this is effectively carrier strength, which is what the squelch keys
// on: an idle airband channel carries no carrier at all, so the level drops to
// the receiver noise floor between transmissions.
func LevelDB(in []complex64) float64 {
	if len(in) == 0 {
		return levelFloorDB
	}
	var sum float64
	for _, s := range in {
		re, im := float64(real(s)), float64(imag(s))
		sum += re*re + im*im
	}

	rms := math.Sqrt(sum / float64(len(in)))
	if rms <= 0 {
		return levelFloorDB
	}
	db := 20 * math.Log10(rms)
	if db < levelFloorDB {
		return levelFloorDB
	}
	return db
}

// Demodulate performs envelope detection: the magnitude of each complex sample.
//
// That is the whole of AM demodulation, and the reason AM airband is so easy to
// receive. The carrier survives as a DC offset, removed next by DCBlock.
func Demodulate(dst []float32, in []complex64) []float32 {
	for _, s := range in {
		re, im := real(s), imag(s)
		dst = append(dst, float32(math.Sqrt(float64(re*re+im*im))))
	}
	return dst
}

// DCBlock is a one-pole high-pass filter that removes the carrier left behind
// by envelope detection.
//
// Its transfer function is (1 - z^-1) / (1 - r*z^-1): a zero at DC to kill the
// offset, and a pole just inside it so everything in the voice band passes
// essentially untouched.
type DCBlock struct {
	r  float32
	x1 float32 // previous input
	y1 float32 // previous output
}

// NewDCBlock returns a high-pass with the given -3 dB corner. Around 30 Hz is
// right for voice: low enough to leave speech alone, high enough to track the
// carrier shifting as a signal fades.
func NewDCBlock(cutoffHz, fs float64) *DCBlock {
	assert.Thatf(fs > 0, "sample rate must be positive, got %v", fs)
	assert.Thatf(cutoffHz > 0 && cutoffHz < fs/2, "cutoff %v out of range for %v", cutoffHz, fs)

	return &DCBlock{r: float32(1 - 2*math.Pi*cutoffHz/fs)}
}

func (d *DCBlock) Reset() {
	d.x1, d.y1 = 0, 0
}

// Process filters x in place.
func (d *DCBlock) Process(x []float32) {
	for i, v := range x {
		y := v - d.x1 + d.r*d.y1
		d.x1, d.y1 = v, y
		x[i] = y
	}
}

// AGCConfig configures automatic gain control.
type AGCConfig struct {
	SampleRate float64
	// Target is the output amplitude the loudest peaks are driven towards.
	Target float32
	// MaxGain bounds amplification so a silent channel is not wound up to
	// full-scale noise.
	MaxGain float32
	// ReleaseTime is how long, in seconds, gain takes to recover after a loud
	// passage. Attack is immediate, so a sudden strong signal never clips.
	ReleaseTime float64
}

// AGC normalises level with a peak-following envelope detector.
//
// Airband needs this badly: a tower a few miles away and an aircraft at
// altitude can differ by 40 dB, and a listener should not have to reach for the
// volume control between transmissions.
type AGC struct {
	target  float32
	maxGain float32
	decay   float32

	env  float32
	gain float32
	hold bool
}

func NewAGC(cfg AGCConfig) *AGC {
	assert.Thatf(cfg.SampleRate > 0, "sample rate must be positive, got %v", cfg.SampleRate)
	assert.Thatf(cfg.Target > 0, "target must be positive, got %v", cfg.Target)

	return &AGC{
		target:  cfg.Target,
		maxGain: cfg.MaxGain,
		decay:   float32(math.Exp(-1 / (cfg.SampleRate * cfg.ReleaseTime))),
		gain:    1,
	}
}

// Gain reports the current gain, for status reporting and tests.
func (a *AGC) Gain() float32 { return a.gain }

// SetHold freezes gain adaptation. The squelch holds the AGC while closed:
// otherwise it would spend every silent gap winding gain up to track receiver
// noise, and the first syllable of the next transmission would arrive
// massively overdriven.
func (a *AGC) SetHold(hold bool) { a.hold = hold }

func (a *AGC) Reset() {
	a.env, a.gain, a.hold = 0, 1, false
}

// Process applies gain to x in place, adapting as it goes.
func (a *AGC) Process(x []float32) {
	const envFloor = 1e-9

	for i, v := range x {
		if !a.hold {
			mag := v
			if mag < 0 {
				mag = -mag
			}
			// Immediate attack, exponential release.
			a.env *= a.decay
			if mag > a.env {
				a.env = mag
			}

			denom := a.env
			if denom < envFloor {
				denom = envFloor
			}
			a.gain = a.target / denom
			if a.gain > a.maxGain {
				a.gain = a.maxGain
			}
		}

		// A hard limiter behind the AGC guarantees the output stays in range
		// even before the envelope has caught up with a sudden signal.
		out := v * a.gain
		switch {
		case out > 1:
			out = 1
		case out < -1:
			out = -1
		}
		x[i] = out
	}
}

// SquelchConfig configures carrier-triggered muting.
type SquelchConfig struct {
	// ThresholdDB is the level at which the channel opens, in dBFS.
	ThresholdDB float64
	// HysteresisDB is how far below the threshold the level must fall before
	// closing, preventing chatter on a marginal signal.
	HysteresisDB float64
	// HangSamples keeps the channel open briefly after the carrier drops, so
	// natural gaps in speech do not chop words apart.
	HangSamples int
}

// Squelch mutes a channel that carries no signal.
//
// This is not a nicety. An airband channel is silent well over 90% of the time,
// and an unsquelched AGC will chase the noise floor to full gain, producing a
// loud hiss that makes the receiver unusable.
//
// Timing is counted in samples rather than wall-clock so behaviour is
// deterministic and testable, and identical whether replaying a capture at
// speed or receiving live.
type Squelch struct {
	openDB  float64
	closeDB float64
	hangLen int

	open bool
	hang int // samples of hang remaining
}

func NewSquelch(cfg SquelchConfig) *Squelch {
	assert.Thatf(cfg.HysteresisDB >= 0, "hysteresis must not be negative, got %v", cfg.HysteresisDB)
	assert.Thatf(cfg.HangSamples >= 0, "hang must not be negative, got %d", cfg.HangSamples)

	return &Squelch{
		openDB:  cfg.ThresholdDB,
		closeDB: cfg.ThresholdDB - cfg.HysteresisDB,
		hangLen: cfg.HangSamples,
	}
}

// Open reports whether the channel is currently passing audio.
func (s *Squelch) Open() bool { return s.open }

func (s *Squelch) Reset() {
	s.open, s.hang = false, 0
}

// Update feeds one block's measured level and the number of samples it covered,
// returning whether the channel should pass audio.
func (s *Squelch) Update(levelDB float64, samples int) bool {
	// Hot path: constant message only, so nothing is boxed. See package assert.
	assert.That(samples >= 0, "squelch sample count must not be negative")

	switch {
	case levelDB >= s.openDB:
		// A clear carrier: open, and restart the hang window.
		s.open = true
		s.hang = s.hangLen

	case !s.open:
		// Still closed and nothing strong enough to open it.

	case levelDB >= s.closeDB:
		// Inside the hysteresis band, so hold open rather than chatter.
		s.hang = s.hangLen

	default:
		// Carrier gone: run down the hang window, then close.
		s.hang -= samples
		if s.hang <= 0 {
			s.open = false
			s.hang = 0
		}
	}
	return s.open
}
