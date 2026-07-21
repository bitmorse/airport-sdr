package dsp

import (
	"math"
	"testing"
)

// carrierWithTone builds an AM signal the way a transmitter does: a carrier of
// the given amplitude, amplitude-modulated by a single audio tone at depth m.
// The envelope is therefore amp*(1 + m*cos(2*pi*audio*t)), which is exactly
// what the demodulator must recover.
func carrierWithTone(n int, amp, m, audio, fs float64) []complex64 {
	out := make([]complex64, n)
	for i := range out {
		t := float64(i) / fs
		env := amp * (1 + m*math.Cos(2*math.Pi*audio*t))
		out[i] = complex(float32(env), 0)
	}
	return out
}

func peakToPeak(x []float32) (lo, hi float32) {
	lo, hi = x[0], x[0]
	for _, v := range x {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return lo, hi
}

// --- level measurement ------------------------------------------------------

func TestLevelDB(t *testing.T) {
	cases := map[string]struct {
		mag  float32
		want float64
	}{
		"full scale": {1.0, 0},
		"minus 20":   {0.1, -20},
		"minus 40":   {0.01, -40},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := make([]complex64, 256)
			for i := range in {
				in[i] = complex(c.mag, 0)
			}
			if got := LevelDB(in); math.Abs(got-c.want) > 0.01 {
				t.Errorf("LevelDB = %.3f, want %.3f", got, c.want)
			}
		})
	}
}

// Silence must produce a very low number, not negative infinity, or it would
// poison every downstream comparison.
func TestLevelDBOfSilenceIsFinite(t *testing.T) {
	got := LevelDB(make([]complex64, 64))
	if math.IsInf(got, 0) || math.IsNaN(got) {
		t.Fatalf("LevelDB of silence = %v, want a finite floor", got)
	}
	if got > -100 {
		t.Errorf("LevelDB of silence = %v, want a very low value", got)
	}
}

// --- AM demodulation --------------------------------------------------------

func TestDemodulateRecoversMagnitude(t *testing.T) {
	in := []complex64{complex(3, 4), complex(-5, 12), complex(1, 0), complex(0, 0)}
	want := []float32{5, 13, 1, 0}

	got := Demodulate(make([]float32, 0, len(in)), in)
	if len(got) != len(want) {
		t.Fatalf("got %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-5 {
			t.Errorf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// The envelope of a modulated carrier must come back with the right DC offset
// and the right modulation depth.
func TestDemodulateRecoversEnvelope(t *testing.T) {
	const amp, depth = 0.5, 0.8
	in := carrierWithTone(2400, amp, depth, 1000, 24_000)

	got := Demodulate(make([]float32, 0, len(in)), in)
	lo, hi := peakToPeak(got)

	if wantHi := amp * (1 + depth); math.Abs(float64(hi)-wantHi) > 1e-3 {
		t.Errorf("envelope peak = %v, want %v", hi, wantHi)
	}
	if wantLo := amp * (1 - depth); math.Abs(float64(lo)-wantLo) > 1e-3 {
		t.Errorf("envelope trough = %v, want %v", lo, wantLo)
	}
}

func TestAllocDemodulate(t *testing.T) {
	in := make([]complex64, 480)
	dst := make([]float32, 0, len(in))
	if n := testing.AllocsPerRun(100, func() {
		dst = Demodulate(dst[:0], in)
	}); n != 0 {
		t.Errorf("Demodulate allocated %v times per run, want 0", n)
	}
}

// --- DC block ---------------------------------------------------------------

// After envelope detection the carrier appears as a large DC offset. Left in
// place it would dominate the audio and drive the AGC entirely.
func TestDCBlockRemovesConstantOffset(t *testing.T) {
	d := NewDCBlock(30, 24_000)
	x := make([]float32, 8000)
	for i := range x {
		x[i] = 0.7
	}
	d.Process(x)

	if got := x[len(x)-1]; math.Abs(float64(got)) > 1e-3 {
		t.Errorf("residual DC = %v, want ~0", got)
	}
}

func TestDCBlockPassesAudioBand(t *testing.T) {
	const fs, freq = 24_000.0, 1000.0
	d := NewDCBlock(30, fs)

	x := make([]float32, 4800)
	for i := range x {
		x[i] = float32(math.Sin(2 * math.Pi * freq * float64(i) / fs))
	}
	d.Process(x)

	// Well above the cutoff the filter must be transparent.
	lo, hi := peakToPeak(x[len(x)/2:])
	if math.Abs(float64(hi)-1) > 0.02 || math.Abs(float64(lo)+1) > 0.02 {
		t.Errorf("1 kHz tone came through as [%v, %v], want ~[-1, 1]", lo, hi)
	}
}

func TestDCBlockRemovesOffsetButKeepsTone(t *testing.T) {
	const fs = 24_000.0
	d := NewDCBlock(30, fs)

	x := make([]float32, 9600)
	for i := range x {
		x[i] = 0.6 + 0.3*float32(math.Sin(2*math.Pi*1000*float64(i)/fs))
	}
	d.Process(x)

	lo, hi := peakToPeak(x[len(x)/2:])
	if mid := (hi + lo) / 2; math.Abs(float64(mid)) > 5e-3 {
		t.Errorf("tone is centred on %v, want ~0 after DC removal", mid)
	}
	if amp := (hi - lo) / 2; math.Abs(float64(amp)-0.3) > 0.01 {
		t.Errorf("tone amplitude = %v, want 0.3", amp)
	}
}

func TestAllocDCBlock(t *testing.T) {
	d := NewDCBlock(30, 24_000)
	x := make([]float32, 480)
	if n := testing.AllocsPerRun(100, func() { d.Process(x) }); n != 0 {
		t.Errorf("DCBlock.Process allocated %v times per run, want 0", n)
	}
}

// --- AGC --------------------------------------------------------------------

func newTestAGC() *AGC {
	return NewAGC(AGCConfig{
		SampleRate:  24_000,
		Target:      0.5,
		MaxGain:     100,
		ReleaseTime: 0.2,
	})
}

// Airband signal strength varies enormously between a tower a mile away and an
// aircraft at altitude, so the AGC has to bring both to a usable level.
func TestAGCConvergesToTarget(t *testing.T) {
	for _, amp := range []float32{0.01, 0.05, 0.2} {
		a := newTestAGC()
		x := make([]float32, 48_000) // two seconds
		for i := range x {
			x[i] = amp * float32(math.Sin(2*math.Pi*1000*float64(i)/24_000))
		}
		a.Process(x)

		_, hi := peakToPeak(x[len(x)-2400:])
		if math.Abs(float64(hi)-0.5) > 0.05 {
			t.Errorf("input amplitude %v settled at peak %v, want ~0.5", amp, hi)
		}
	}
}

func TestAGCNeverExceedsFullScale(t *testing.T) {
	a := newTestAGC()
	x := make([]float32, 24_000)
	for i := range x {
		x[i] = 8 * float32(math.Sin(2*math.Pi*1000*float64(i)/24_000)) // badly overdriven
	}
	a.Process(x)

	for i, v := range x {
		if v > 1 || v < -1 {
			t.Fatalf("sample %d = %v, outside [-1, 1]", i, v)
		}
	}
}

func TestAGCRespectsMaxGain(t *testing.T) {
	a := NewAGC(AGCConfig{SampleRate: 24_000, Target: 0.5, MaxGain: 4, ReleaseTime: 0.05})
	x := make([]float32, 24_000)
	for i := range x {
		x[i] = 0.001
	}
	a.Process(x)

	if got := x[len(x)-1]; got > 0.001*4*1.01 {
		t.Errorf("output %v exceeds input * MaxGain", got)
	}
}

// While the squelch is closed the input is receiver noise. Letting the AGC
// track it would wind gain to maximum, so the first syllable of the next
// transmission arrives massively overdriven.
func TestAGCHoldFreezesGain(t *testing.T) {
	a := newTestAGC()
	loud := make([]float32, 24_000)
	for i := range loud {
		loud[i] = 0.5 * float32(math.Sin(2*math.Pi*1000*float64(i)/24_000))
	}
	a.Process(loud)
	settled := a.Gain()

	a.SetHold(true)
	noise := make([]float32, 48_000)
	for i := range noise {
		noise[i] = 0.0001
	}
	a.Process(noise)

	if math.Abs(float64(a.Gain()-settled)) > 1e-6 {
		t.Errorf("gain moved from %v to %v while held", settled, a.Gain())
	}
}

func TestAllocAGC(t *testing.T) {
	a := newTestAGC()
	x := make([]float32, 480)
	if n := testing.AllocsPerRun(100, func() { a.Process(x) }); n != 0 {
		t.Errorf("AGC.Process allocated %v times per run, want 0", n)
	}
}

// --- squelch ----------------------------------------------------------------

func newTestSquelch() *Squelch {
	return NewSquelch(SquelchConfig{
		ThresholdDB:  -35,
		HysteresisDB: 3,
		HangSamples:  2400, // 100 ms at 24 kHz
	})
}

func TestSquelchStartsClosed(t *testing.T) {
	if newTestSquelch().Open() {
		t.Error("squelch must start closed; airband is silent most of the time")
	}
}

func TestSquelchOpensAboveThreshold(t *testing.T) {
	s := newTestSquelch()
	if !s.Update(-20, 480) {
		t.Error("a signal well above threshold must open the squelch")
	}
}

func TestSquelchStaysClosedBelowThreshold(t *testing.T) {
	s := newTestSquelch()
	if s.Update(-60, 480) {
		t.Error("noise below threshold must not open the squelch")
	}
}

// Without hysteresis a signal hovering at the threshold chatters the squelch
// open and shut, which is far more unpleasant to listen to than plain noise.
func TestSquelchHysteresis(t *testing.T) {
	s := newTestSquelch()
	s.Update(-20, 480) // open

	// Between the close threshold (-38) and the open threshold (-35) it must
	// remain open once opened.
	if !s.Update(-36, 480) {
		t.Error("squelch closed inside the hysteresis band")
	}
	// Below the close threshold, and past the hang time, it must close.
	s.Update(-50, 480)
	if s.Update(-50, 4800) {
		t.Error("squelch stayed open well below the close threshold")
	}
}

// Speech has natural gaps. Closing instantly on every pause chops words apart,
// so the squelch holds open briefly after the signal drops.
func TestSquelchHangTime(t *testing.T) {
	s := newTestSquelch()
	s.Update(-20, 480)

	if !s.Update(-80, 1200) { // 50 ms into a 100 ms hang
		t.Error("squelch closed during the hang window")
	}
	if s.Update(-80, 2400) { // past the hang window
		t.Error("squelch failed to close after the hang window expired")
	}
}

func TestSquelchReopensDuringHang(t *testing.T) {
	s := newTestSquelch()
	s.Update(-20, 480)
	s.Update(-80, 1200) // hang started
	s.Update(-20, 480)  // signal returns

	if !s.Update(-80, 1200) {
		t.Error("hang window must restart when the signal returns")
	}
}

func TestAllocSquelch(t *testing.T) {
	s := newTestSquelch()
	if n := testing.AllocsPerRun(100, func() { s.Update(-30, 480) }); n != 0 {
		t.Errorf("Squelch.Update allocated %v times per run, want 0", n)
	}
}
