package dsp

import (
	"math"
	"testing"
)

func magAt(x []complex64, i int) float64 {
	return math.Hypot(float64(real(x[i])), float64(imag(x[i])))
}

// peakBin returns the index of the largest magnitude, which is what every
// spectrum assertion below comes down to.
func peakBin(x []complex64) int {
	best, bestMag := 0, -1.0
	for i := range x {
		if m := magAt(x, i); m > bestMag {
			best, bestMag = i, m
		}
	}
	return best
}

func TestFFTPlanRejectsNonPowerOfTwo(t *testing.T) {
	for _, n := range []int{0, 3, 100, 1000} {
		if _, err := NewFFTPlan(n); err == nil {
			t.Errorf("NewFFTPlan(%d) should fail: radix-2 needs a power of two", n)
		}
	}
}

func TestFFTPlanAcceptsPowerOfTwo(t *testing.T) {
	for _, n := range []int{2, 4, 256, 4096} {
		if _, err := NewFFTPlan(n); err != nil {
			t.Errorf("NewFFTPlan(%d): %v", n, err)
		}
	}
}

// A constant signal is pure DC, so all its energy belongs in bin zero.
func TestFFTOfConstantIsAllInBinZero(t *testing.T) {
	p, _ := NewFFTPlan(64)
	x := make([]complex64, 64)
	for i := range x {
		x[i] = complex(1, 0)
	}
	p.Transform(x)

	if got := magAt(x, 0); math.Abs(got-64) > 1e-3 {
		t.Errorf("bin 0 magnitude = %v, want 64", got)
	}
	for i := 1; i < len(x); i++ {
		if m := magAt(x, i); m > 1e-3 {
			t.Errorf("bin %d = %v, want ~0", i, m)
		}
	}
}

// A complex exponential at exactly bin k must land in bin k and nowhere else.
// This is the property the whole spectrum display rests on.
func TestFFTPutsToneInItsOwnBin(t *testing.T) {
	const n = 256
	for _, k := range []int{1, 7, 64, 200} {
		p, _ := NewFFTPlan(n)
		x := make([]complex64, n)
		for i := range x {
			th := 2 * math.Pi * float64(k) * float64(i) / n
			x[i] = complex(float32(math.Cos(th)), float32(math.Sin(th)))
		}
		p.Transform(x)

		if got := peakBin(x); got != k {
			t.Errorf("tone at bin %d peaked in bin %d", k, got)
		}
		if got := magAt(x, k); math.Abs(got-n) > 0.5 {
			t.Errorf("bin %d magnitude = %v, want ~%d", k, got, n)
		}
	}
}

// Negative frequencies live in the upper half of the output. Getting this
// backwards would mirror the whole spectrum display left-to-right.
func TestFFTNegativeFrequencyLandsInUpperHalf(t *testing.T) {
	const n = 256
	p, _ := NewFFTPlan(n)
	x := make([]complex64, n)
	for i := range x {
		th := -2 * math.Pi * 10 * float64(i) / n
		x[i] = complex(float32(math.Cos(th)), float32(math.Sin(th)))
	}
	p.Transform(x)

	if got := peakBin(x); got != n-10 {
		t.Errorf("tone at -10 bins peaked in bin %d, want %d", got, n-10)
	}
}

// Parseval's theorem: the transform must conserve energy. This catches scaling
// and butterfly mistakes that a single-tone test can miss.
func TestFFTConservesEnergy(t *testing.T) {
	const n = 128
	p, _ := NewFFTPlan(n)

	x := make([]complex64, n)
	for i := range x {
		x[i] = complex(float32(math.Sin(float64(i))), float32(math.Cos(float64(i)*0.7)))
	}

	var timeEnergy float64
	for _, v := range x {
		timeEnergy += float64(real(v))*float64(real(v)) + float64(imag(v))*float64(imag(v))
	}
	p.Transform(x)

	var freqEnergy float64
	for _, v := range x {
		freqEnergy += float64(real(v))*float64(real(v)) + float64(imag(v))*float64(imag(v))
	}
	freqEnergy /= n

	if math.Abs(timeEnergy-freqEnergy) > timeEnergy*1e-4 {
		t.Errorf("energy %v in time domain became %v in frequency domain",
			timeEnergy, freqEnergy)
	}
}

func TestFFTRejectsWrongLength(t *testing.T) {
	p, _ := NewFFTPlan(64)
	defer func() {
		if recover() == nil {
			t.Error("transforming a slice of the wrong length must not be silently wrong")
		}
	}()
	p.Transform(make([]complex64, 32))
}

// --- averaged spectrum ------------------------------------------------------

func TestSpectrumFindsToneAtItsFrequency(t *testing.T) {
	const (
		fs     = 960_000.0
		offset = -150_000.0 // where 118.1 sits when centred on 118.25
	)
	s, err := NewSpectrum(4096, fs)
	if err != nil {
		t.Fatal(err)
	}

	in := tone(4096*8, offset, fs)
	for off := 0; off+4096 <= len(in); off += 4096 {
		s.Accumulate(in[off : off+4096])
	}

	freq, _ := s.Peak()
	if math.Abs(freq-offset) > fs/4096*2 {
		t.Errorf("peak at %.0f Hz offset, want %.0f", freq, offset)
	}
}

// The display puts negative offsets on the left, so the returned slice must run
// from -fs/2 to +fs/2 rather than in raw FFT order.
func TestSpectrumIsOrderedLowToHighFrequency(t *testing.T) {
	const fs = 960_000.0
	s, _ := NewSpectrum(1024, fs)
	s.Accumulate(tone(1024, -300_000, fs))

	power := s.PowerDB()
	if len(power) != 1024 {
		t.Fatalf("got %d bins, want 1024", len(power))
	}
	if got := s.BinFreq(0); got > -fs/2+1 {
		t.Errorf("first bin is %v Hz, want about %v", got, -fs/2)
	}
	if got := s.BinFreq(1023); got < fs/2-fs/1024-1 {
		t.Errorf("last bin is %v Hz, want about %v", got, fs/2)
	}

	// A tone below centre must appear in the lower half of the ordered output.
	if freq, _ := s.Peak(); freq > 0 {
		t.Errorf("tone at -300 kHz reported at %v Hz", freq)
	}
}

// Airband traffic is intermittent: a five-second transmission inside a minute
// of silence all but vanishes from an average, but stands out in a max-hold.
// This is the difference between seeing the traffic and concluding the band is
// dead.
func TestSpectrumMaxHoldCatchesTransientSignal(t *testing.T) {
	const (
		fs     = 960_000.0
		size   = 1024
		offset = -150_000.0
	)
	s, _ := NewSpectrum(size, fs)

	burst := tone(size, offset, fs)
	quiet := make([]complex64, size)
	for i := range quiet {
		quiet[i] = complex(1e-5, 0)
	}

	// One frame in sixteen carries the signal.
	s.Accumulate(burst)
	for i := 0; i < 15; i++ {
		s.Accumulate(quiet)
	}

	bin := int(offset/(fs/size)) + size/2
	avg, maxHold := s.PowerDB(), s.MaxHoldDB()

	if maxHold[bin] <= avg[bin] {
		t.Errorf("max-hold (%.1f dB) should exceed the average (%.1f dB) for a burst",
			maxHold[bin], avg[bin])
	}
	// The averaged level is diluted by the sixteen-frame window, about 12 dB.
	if diff := maxHold[bin] - avg[bin]; diff < 6 {
		t.Errorf("max-hold is only %.1f dB above the average; the burst is being lost", diff)
	}
}

func TestSpectrumWithNoInput(t *testing.T) {
	s, _ := NewSpectrum(256, 960_000)
	if got := s.PowerDB(); len(got) != 256 {
		t.Errorf("got %d bins before any input, want 256", len(got))
	}
}
