// Package dsp implements the receive chain: frequency shift, decimation,
// demodulation, squelch and gain control.
//
// Every stage is a plain struct with a Process method that appends to a
// caller-supplied slice. Nothing here allocates once constructed (Power of 10
// rule 3), and nothing here knows about hardware, so the whole chain can be
// driven from a recorded capture or a synthesised signal.
package dsp

import (
	"math"

	"github.com/bitmorse/airport-sdr/internal/assert"
)

// blackmanTransitionFactor relates a Blackman window's tap count to the
// transition width it achieves: taps ~= factor * fs / transition. The window
// buys roughly 74 dB of stopband attenuation, which is ample once decimation
// folds the stopband back over the signal.
const blackmanTransitionFactor = 5.5

// TapsForTransition estimates the tap count needed to fall from passband to
// stopband within the given width. The result is always odd, so the filter has
// an integer group delay.
func TapsForTransition(fs, transition float64) int {
	assert.Thatf(fs > 0, "sample rate must be positive, got %v", fs)
	assert.Thatf(transition > 0, "transition width must be positive, got %v", transition)

	taps := int(math.Ceil(blackmanTransitionFactor * fs / transition))
	if taps < 3 {
		taps = 3
	}
	if taps%2 == 0 {
		taps++
	}
	return taps
}

// DesignLowPass returns a windowed-sinc low-pass kernel normalised to unity DC
// gain. taps is rounded up to the next odd number if necessary.
func DesignLowPass(fs, cutoff float64, taps int) []float32 {
	assert.Thatf(fs > 0, "sample rate must be positive, got %v", fs)
	assert.Thatf(cutoff > 0 && cutoff < fs/2, "cutoff %v must be within (0, %v)", cutoff, fs/2)

	if taps%2 == 0 {
		taps++
	}
	kernel := make([]float32, taps)

	fc := cutoff / fs // cycles per sample
	m := float64(taps - 1)
	var sum float64

	for i := 0; i < taps; i++ {
		n := float64(i) - m/2

		h := 2 * fc // the limit of the sinc at n == 0
		if n != 0 {
			h = math.Sin(2*math.Pi*fc*n) / (math.Pi * n)
		}
		// Blackman window: ~74 dB stopband for a modest tap count.
		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(i)/m) + 0.08*math.Cos(4*math.Pi*float64(i)/m)

		h *= w
		kernel[i] = float32(h)
		sum += h
	}

	assert.That(sum != 0, "degenerate kernel: taps sum to zero")
	for i := range kernel {
		kernel[i] = float32(float64(kernel[i]) / sum)
	}
	return kernel
}

// FIRDecimC is a decimating FIR filter over complex samples.
//
// It is polyphase in the sense that matters: only the samples that survive
// decimation are ever computed. Filtering at the input rate and throwing most
// of the results away costs the decimation factor in wasted multiplies, which
// on a Pi-class device is the difference between viable and not.
type FIRDecimC struct {
	taps  []float32
	decim int

	// hist holds the trailing len(taps)-1 samples of the previous call, so
	// that output is independent of how the input was chunked.
	hist  []complex64
	phase int // index of the next output within hist ++ in
}

func NewFIRDecimC(taps []float32, decim int) *FIRDecimC {
	assert.That(len(taps) > 0, "filter needs at least one tap")
	assert.Thatf(decim > 0, "decimation must be positive, got %d", decim)

	return &FIRDecimC{
		taps:  taps,
		decim: decim,
		hist:  make([]complex64, len(taps)-1),
	}
}

// MaxOutputLen is an upper bound on the samples Process will append for an
// input of inLen, for sizing destination buffers up front.
func (f *FIRDecimC) MaxOutputLen(inLen int) int { return inLen/f.decim + 1 }

// Reset clears the filter's memory.
func (f *FIRDecimC) Reset() {
	for i := range f.hist {
		f.hist[i] = 0
	}
	f.phase = 0
}

// Process filters and decimates in, appending to dst and returning the result.
// It allocates nothing provided dst has spare capacity; use MaxOutputLen.
func (f *FIRDecimC) Process(dst, in []complex64) []complex64 {
	ntaps := len(f.taps)
	h := len(f.hist) // ntaps-1
	n := len(in)

	// The filter sees the virtual array hist ++ in. Reading across the seam
	// costs a branch per tap and saves copying every sample into a scratch
	// buffer, which at 960 kS/s is the better trade.
	i := f.phase
	for ; i+ntaps <= h+n; i += f.decim {
		var re, im float32
		for j := 0; j < ntaps; j++ {
			var s complex64
			if k := i + j; k < h {
				s = f.hist[k]
			} else {
				s = in[k-h]
			}
			t := f.taps[j]
			re += real(s) * t
			im += imag(s) * t
		}
		dst = append(dst, complex(re, im))
	}

	f.retain(in, i)
	return dst
}

// retain keeps the trailing len(hist) samples for the next call and rebases the
// output phase onto them.
func (f *FIRDecimC) retain(in []complex64, next int) {
	h, n := len(f.hist), len(in)

	// Copying forward is safe: every read index is ahead of its write index.
	for k := 0; k < h; k++ {
		if src := n + k; src < h {
			f.hist[k] = f.hist[src]
		} else {
			f.hist[k] = in[src-h]
		}
	}

	// Hot path: constant message only, so nothing is boxed. See package assert.
	assert.That(next >= n, "FIR output index fell behind the input length")
	f.phase = next - n
}

// FIRDecimR is the real-valued counterpart of FIRDecimC, used after
// demodulation where the signal is no longer complex.
type FIRDecimR struct {
	taps  []float32
	decim int
	hist  []float32
	phase int
}

func NewFIRDecimR(taps []float32, decim int) *FIRDecimR {
	assert.That(len(taps) > 0, "filter needs at least one tap")
	assert.Thatf(decim > 0, "decimation must be positive, got %d", decim)

	return &FIRDecimR{
		taps:  taps,
		decim: decim,
		hist:  make([]float32, len(taps)-1),
	}
}

func (f *FIRDecimR) MaxOutputLen(inLen int) int { return inLen/f.decim + 1 }

func (f *FIRDecimR) Reset() {
	for i := range f.hist {
		f.hist[i] = 0
	}
	f.phase = 0
}

// Process filters and decimates in, appending to dst and returning the result.
func (f *FIRDecimR) Process(dst, in []float32) []float32 {
	ntaps := len(f.taps)
	h := len(f.hist)
	n := len(in)

	i := f.phase
	for ; i+ntaps <= h+n; i += f.decim {
		var acc float32
		for j := 0; j < ntaps; j++ {
			var s float32
			if k := i + j; k < h {
				s = f.hist[k]
			} else {
				s = in[k-h]
			}
			acc += s * f.taps[j]
		}
		dst = append(dst, acc)
	}

	for k := 0; k < h; k++ {
		if src := n + k; src < h {
			f.hist[k] = f.hist[src]
		} else {
			f.hist[k] = in[src-h]
		}
	}
	// Hot path: constant message only, so nothing is boxed. See package assert.
	assert.That(i >= n, "FIR output index fell behind the input length")
	f.phase = i - n

	return dst
}
