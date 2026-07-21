package dsp

import (
	"math"
	"math/cmplx"

	"github.com/bitmorse/airport-sdr/internal/assert"
)

// ncoRenormInterval is how often the phasor is pushed back onto the unit
// circle. Advancing by repeated multiplication drifts by roughly 1e-16 per
// step, which is negligible over a thousand samples and unacceptable over the
// billions a device sees between restarts.
const ncoRenormInterval = 1024

// NCO shifts a complex baseband signal in frequency by multiplying it with a
// rotating phasor.
//
// This is how a channel is selected: the tuner is parked away from every
// channel of interest to keep them off the DC spur, and each channel is then
// shifted down to zero here before decimation narrows in on it.
type NCO struct {
	shift float64
	fs    float64

	step   complex128 // per-sample rotation
	phasor complex128
	count  int
}

// NewNCO returns an oscillator that shifts a signal by shiftHz. To bring a
// channel sitting at +offset down to DC, pass -offset.
func NewNCO(shiftHz, fs float64) *NCO {
	assert.Thatf(fs > 0, "sample rate must be positive, got %v", fs)
	assert.Thatf(math.Abs(shiftHz) <= fs/2, "shift %v exceeds Nyquist for %v", shiftHz, fs)

	n := &NCO{
		shift: shiftHz,
		fs:    fs,
		step:  cmplx.Exp(complex(0, 2*math.Pi*shiftHz/fs)),
	}
	n.Reset()
	return n
}

// Reset returns the oscillator to zero phase.
func (n *NCO) Reset() {
	n.phasor = complex(1, 0)
	n.count = 0
}

// Mix shifts in by the configured frequency, appending to dst and returning the
// result. It allocates nothing provided dst has capacity for len(in) samples.
func (n *NCO) Mix(dst, in []complex64) []complex64 {
	for _, s := range in {
		dst = append(dst, s*complex64(n.phasor))

		n.phasor *= n.step
		n.count++
		if n.count >= ncoRenormInterval {
			n.renormalise()
		}
	}
	return dst
}

// renormalise divides out any magnitude the phasor has accumulated.
func (n *NCO) renormalise() {
	mag := cmplx.Abs(n.phasor)
	// Hot path: constant message only, so nothing is boxed. See package assert.
	assert.That(mag > 0.5 && mag < 2, "NCO phasor magnitude has diverged")

	n.phasor /= complex(mag, 0)
	n.count = 0
}
