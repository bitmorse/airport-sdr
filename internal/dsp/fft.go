package dsp

import (
	"fmt"
	"math"
	"math/cmplx"
)

// This file is diagnostic machinery, not part of the receive chain. It exists
// to answer the question the audio cannot: is there actually a carrier where we
// think there is, or is the receiver listening to nothing? Nothing here runs
// per-block during normal operation, so it is written for clarity rather than
// for the allocation discipline the rest of the package follows.

// FFTPlan performs an in-place radix-2 FFT of a fixed size. The twiddle factors
// and bit-reversal indices are computed once, which keeps repeated transforms
// accurate: deriving twiddles by successive multiplication accumulates error
// that shows up as a raised noise floor across the display.
type FFTPlan struct {
	n        int
	twiddles []complex64
	reversed []int
}

// NewFFTPlan prepares transforms of exactly n points, which must be a power of
// two.
func NewFFTPlan(n int) (*FFTPlan, error) {
	if n < 2 || n&(n-1) != 0 {
		return nil, fmt.Errorf("FFT size must be a power of two of at least 2, got %d", n)
	}

	p := &FFTPlan{
		n:        n,
		twiddles: make([]complex64, n/2),
		reversed: make([]int, n),
	}
	for k := range p.twiddles {
		p.twiddles[k] = complex64(cmplx.Exp(complex(0, -2*math.Pi*float64(k)/float64(n))))
	}

	bits := 0
	for 1<<bits < n {
		bits++
	}
	for i := 0; i < n; i++ {
		r := 0
		for b := 0; b < bits; b++ {
			if i&(1<<b) != 0 {
				r |= 1 << (bits - 1 - b)
			}
		}
		p.reversed[i] = r
	}
	return p, nil
}

// Size reports the transform length.
func (p *FFTPlan) Size() int { return p.n }

// Transform replaces x with its discrete Fourier transform. It panics if x is
// not exactly Size() long, because a quietly truncated transform would produce
// a plausible-looking but wrong spectrum.
func (p *FFTPlan) Transform(x []complex64) {
	if len(x) != p.n {
		panic(fmt.Sprintf("FFT plan is for %d points, got %d", p.n, len(x)))
	}

	for i, r := range p.reversed {
		if i < r {
			x[i], x[r] = x[r], x[i]
		}
	}

	for size := 2; size <= p.n; size <<= 1 {
		half := size / 2
		step := p.n / size
		for start := 0; start < p.n; start += size {
			for j := 0; j < half; j++ {
				a := x[start+j]
				b := x[start+j+half] * p.twiddles[j*step]
				x[start+j] = a + b
				x[start+j+half] = a - b
			}
		}
	}
}

// Spectrum accumulates an averaged power spectrum across many blocks.
//
// Averaging matters: a single transform of receiver noise is so ragged that a
// weak carrier is invisible in it. Averaging over a whole capture pulls the
// noise down and leaves anything steady standing clearly above it.
type Spectrum struct {
	plan    *FFTPlan
	fs      float64
	window  []float32
	scratch []complex64
	power   []float64
	// peak keeps the strongest value each bin has ever reached. Airband traffic
	// is intermittent, and a transmission lasting seconds is averaged almost to
	// nothing over a minute-long capture; max-hold is what makes it visible.
	peak   []float64
	frames int
}

// NewSpectrum prepares an averaged spectrum of size bins over a span of fs.
func NewSpectrum(size int, fs float64) (*Spectrum, error) {
	plan, err := NewFFTPlan(size)
	if err != nil {
		return nil, err
	}
	if fs <= 0 {
		return nil, fmt.Errorf("sample rate must be positive, got %v", fs)
	}

	s := &Spectrum{
		plan:    plan,
		fs:      fs,
		window:  make([]float32, size),
		scratch: make([]complex64, size),
		power:   make([]float64, size),
		peak:    make([]float64, size),
	}
	// A Hann window keeps a strong carrier from smearing across the display and
	// burying weaker signals beside it.
	for i := range s.window {
		s.window[i] = float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(size-1)))
	}
	return s, nil
}

// Accumulate adds one block, which must be exactly the spectrum size.
func (s *Spectrum) Accumulate(block []complex64) {
	for i, v := range block[:s.plan.n] {
		w := s.window[i]
		s.scratch[i] = complex(real(v)*w, imag(v)*w)
	}
	s.plan.Transform(s.scratch)

	for i, v := range s.scratch {
		re, im := float64(real(v)), float64(imag(v))
		p := re*re + im*im
		s.power[i] += p
		if p > s.peak[i] {
			s.peak[i] = p
		}
	}
	s.frames++
}

// PowerDB returns the averaged spectrum in dB, ordered from -fs/2 to +fs/2 so
// that it can be printed left to right as a frequency axis.
func (s *Spectrum) PowerDB() []float64 {
	out := make([]float64, s.plan.n)
	half := s.plan.n / 2
	frames := math.Max(float64(s.frames), 1)
	norm := frames * float64(s.plan.n) * float64(s.plan.n)

	for i := range out {
		// Rotate so that the negative frequencies in the FFT's upper half come
		// first, which is the order a human expects to read.
		src := (i + half) % s.plan.n
		p := s.power[src] / norm
		if p <= 0 {
			out[i] = levelFloorDB
			continue
		}
		if out[i] = 10 * math.Log10(p); out[i] < levelFloorDB {
			out[i] = levelFloorDB
		}
	}
	return out
}

// MaxHoldDB returns the strongest level each bin ever reached, in the same
// low-to-high frequency order as PowerDB. Use it to find intermittent signals
// that averaging would hide.
func (s *Spectrum) MaxHoldDB() []float64 {
	out := make([]float64, s.plan.n)
	half := s.plan.n / 2
	norm := float64(s.plan.n) * float64(s.plan.n)

	for i := range out {
		p := s.peak[(i+half)%s.plan.n] / norm
		if p <= 0 {
			out[i] = levelFloorDB
			continue
		}
		if out[i] = 10 * math.Log10(p); out[i] < levelFloorDB {
			out[i] = levelFloorDB
		}
	}
	return out
}

// BinFreq returns the frequency offset from centre, in hertz, of ordered bin i.
func (s *Spectrum) BinFreq(i int) float64 {
	return (float64(i) - float64(s.plan.n)/2) * s.fs / float64(s.plan.n)
}

// Peak returns the frequency offset and level of the strongest bin.
func (s *Spectrum) Peak() (freq, db float64) {
	power := s.PowerDB()
	best, bestDB := 0, math.Inf(-1)
	for i, v := range power {
		if v > bestDB {
			best, bestDB = i, v
		}
	}
	return s.BinFreq(best), bestDB
}

// Bins reports the number of frequency bins.
func (s *Spectrum) Bins() int { return s.plan.n }
