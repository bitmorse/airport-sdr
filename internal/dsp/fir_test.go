package dsp

import (
	"math"
	"math/cmplx"
	"testing"
)

// responseDB evaluates the filter's frequency response at f, in decibels. A FIR
// kernel's response is just the DFT of its taps, so this needs no library and
// gives an exact answer to compare the design against.
func responseDB(taps []float32, fs, f float64) float64 {
	var sum complex128
	for n, t := range taps {
		theta := -2 * math.Pi * f * float64(n) / fs
		sum += complex(float64(t), 0) * cmplx.Exp(complex(0, theta))
	}
	return 20 * math.Log10(cmplx.Abs(sum))
}

func closeC(a, b complex64, tol float32) bool {
	return float32(math.Abs(float64(real(a)-real(b)))) <= tol &&
		float32(math.Abs(float64(imag(a)-imag(b)))) <= tol
}

// --- kernel design ----------------------------------------------------------

func TestTapsForTransitionIsOdd(t *testing.T) {
	// An odd tap count gives an integer group delay, which keeps the filter
	// linear-phase without a half-sample offset.
	for _, transition := range []float64{1000, 4000, 20000} {
		if n := TapsForTransition(96_000, transition); n%2 == 0 {
			t.Errorf("TapsForTransition(96000, %v) = %d, want odd", transition, n)
		}
	}
}

func TestTapsForTransitionNarrowerNeedsMoreTaps(t *testing.T) {
	wide := TapsForTransition(96_000, 20_000)
	narrow := TapsForTransition(96_000, 2_000)
	if narrow <= wide {
		t.Errorf("narrow transition needs more taps: got %d vs %d", narrow, wide)
	}
}

func TestDesignLowPassHasUnityDCGain(t *testing.T) {
	taps := DesignLowPass(96_000, 8_000, 101)
	var sum float64
	for _, v := range taps {
		sum += float64(v)
	}
	if math.Abs(sum-1) > 1e-6 {
		t.Errorf("tap sum = %v, want 1 (unity DC gain)", sum)
	}
}

func TestDesignLowPassIsSymmetric(t *testing.T) {
	taps := DesignLowPass(96_000, 8_000, 101)
	for i := range taps {
		mirror := taps[len(taps)-1-i]
		if math.Abs(float64(taps[i]-mirror)) > 1e-7 {
			t.Fatalf("tap %d (%v) != mirror (%v); kernel is not linear-phase", i, taps[i], mirror)
		}
	}
}

// The response is what actually matters: passband flat, stopband deep enough
// that aliasing after decimation stays below the noise floor.
func TestDesignLowPassFrequencyResponse(t *testing.T) {
	const (
		fs         = 96_000.0
		cutoff     = 8_000.0
		transition = 4_000.0
	)
	taps := DesignLowPass(fs, cutoff, TapsForTransition(fs, transition))

	if got := responseDB(taps, fs, 0); math.Abs(got) > 0.01 {
		t.Errorf("DC response = %.3f dB, want ~0", got)
	}
	if got := responseDB(taps, fs, cutoff/2); math.Abs(got) > 0.5 {
		t.Errorf("passband response at %v Hz = %.3f dB, want ~0", cutoff/2, got)
	}
	if got := responseDB(taps, fs, cutoff+transition); got > -60 {
		t.Errorf("stopband response at %v Hz = %.1f dB, want < -60",
			cutoff+transition, got)
	}
	if got := responseDB(taps, fs, fs/2); got > -60 {
		t.Errorf("Nyquist response = %.1f dB, want < -60", got)
	}
}

func TestDesignLowPassForcesOddTaps(t *testing.T) {
	if n := len(DesignLowPass(96_000, 8_000, 100)); n%2 == 0 {
		t.Errorf("got %d taps, want odd", n)
	}
}

// --- complex decimating FIR -------------------------------------------------

func TestFIRDecimCIdentityKernel(t *testing.T) {
	f := NewFIRDecimC([]float32{1}, 1)
	in := []complex64{1 + 1i, 2 + 2i, 3 + 3i, 4 + 4i}

	got := f.Process(make([]complex64, 0, 8), in)
	if len(got) != len(in) {
		t.Fatalf("got %d samples, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("sample %d = %v, want %v", i, got[i], in[i])
		}
	}
}

func TestFIRDecimCAppliesDCGain(t *testing.T) {
	// A constant input through a unity-DC-gain kernel must come out unchanged.
	taps := DesignLowPass(96_000, 8_000, 31)
	f := NewFIRDecimC(taps, 1)

	in := make([]complex64, 256)
	for i := range in {
		in[i] = complex(2, -3)
	}
	got := f.Process(make([]complex64, 0, 512), in)

	// Skip the leading edge, where the filter is still filling.
	for i := len(taps); i < len(got); i++ {
		if !closeC(got[i], complex(2, -3), 1e-5) {
			t.Fatalf("steady-state sample %d = %v, want (2,-3)", i, got[i])
		}
	}
}

func TestFIRDecimCDecimates(t *testing.T) {
	f := NewFIRDecimC([]float32{1}, 4)
	in := make([]complex64, 400)
	for i := range in {
		in[i] = complex(float32(i), 0)
	}

	got := f.Process(make([]complex64, 0, 200), in)
	if len(got) != 100 {
		t.Fatalf("decimating 400 samples by 4 gave %d outputs, want 100", len(got))
	}
	// With a single unity tap, output n must be input 4n.
	for i, v := range got {
		if want := complex(float32(i*4), 0); v != want {
			t.Fatalf("output %d = %v, want %v", i, v, want)
		}
	}
}

// The single most important property of a stateful filter: the result must not
// depend on how the input was chunked. A block-boundary bug produces audio that
// sounds almost right, which is the worst kind of wrong.
func TestFIRDecimCChunkingDoesNotChangeResult(t *testing.T) {
	taps := DesignLowPass(96_000, 8_000, 63)
	in := make([]complex64, 1000)
	for i := range in {
		phase := 2 * math.Pi * 3000 * float64(i) / 96_000
		in[i] = complex(float32(math.Cos(phase)), float32(math.Sin(phase)))
	}

	whole := NewFIRDecimC(taps, 5).Process(make([]complex64, 0, 256), in)

	chunked := make([]complex64, 0, 256)
	f := NewFIRDecimC(taps, 5)
	for _, size := range []int{1, 7, 64, 3, 200, 725} {
		chunked = f.Process(chunked, in[:size])
		in = in[size:]
	}

	if len(chunked) != len(whole) {
		t.Fatalf("chunked produced %d samples, whole produced %d", len(chunked), len(whole))
	}
	for i := range whole {
		if !closeC(whole[i], chunked[i], 1e-6) {
			t.Fatalf("sample %d differs: whole %v, chunked %v", i, whole[i], chunked[i])
		}
	}
}

func TestFIRDecimCResetClearsHistory(t *testing.T) {
	taps := DesignLowPass(96_000, 8_000, 31)
	in := make([]complex64, 128)
	for i := range in {
		in[i] = complex(1, 0)
	}

	f := NewFIRDecimC(taps, 2)
	first := append([]complex64(nil), f.Process(make([]complex64, 0, 128), in)...)
	f.Reset()
	second := f.Process(make([]complex64, 0, 128), in)

	for i := range first {
		if !closeC(first[i], second[i], 1e-6) {
			t.Fatalf("after Reset, sample %d = %v, want %v", i, second[i], first[i])
		}
	}
}

func TestAllocFIRDecimC(t *testing.T) {
	taps := DesignLowPass(960_000, 20_000, 127)
	f := NewFIRDecimC(taps, 10)
	in := make([]complex64, 1024)
	dst := make([]complex64, 0, f.MaxOutputLen(len(in)))

	if n := testing.AllocsPerRun(100, func() {
		dst = f.Process(dst[:0], in)
	}); n != 0 {
		t.Errorf("FIRDecimC.Process allocated %v times per run, want 0", n)
	}
}

// --- real decimating FIR ----------------------------------------------------

func TestFIRDecimRMatchesComplexOnRealInput(t *testing.T) {
	taps := DesignLowPass(24_000, 3_400, 63)
	realIn := make([]float32, 500)
	cplxIn := make([]complex64, 500)
	for i := range realIn {
		v := float32(math.Sin(2 * math.Pi * 800 * float64(i) / 24_000))
		realIn[i] = v
		cplxIn[i] = complex(v, 0)
	}

	gotR := NewFIRDecimR(taps, 3).Process(make([]float32, 0, 256), realIn)
	gotC := NewFIRDecimC(taps, 3).Process(make([]complex64, 0, 256), cplxIn)

	if len(gotR) != len(gotC) {
		t.Fatalf("real gave %d samples, complex gave %d", len(gotR), len(gotC))
	}
	for i := range gotR {
		if math.Abs(float64(gotR[i]-real(gotC[i]))) > 1e-6 {
			t.Fatalf("sample %d: real %v, complex %v", i, gotR[i], real(gotC[i]))
		}
	}
}

func TestAllocFIRDecimR(t *testing.T) {
	f := NewFIRDecimR(DesignLowPass(24_000, 3_400, 63), 3)
	in := make([]float32, 480)
	dst := make([]float32, 0, f.MaxOutputLen(len(in)))

	if n := testing.AllocsPerRun(100, func() {
		dst = f.Process(dst[:0], in)
	}); n != 0 {
		t.Errorf("FIRDecimR.Process allocated %v times per run, want 0", n)
	}
}

func BenchmarkFIRDecimC(b *testing.B) {
	f := NewFIRDecimC(DesignLowPass(960_000, 20_000, 127), 10)
	in := make([]complex64, 19_200) // 20 ms at 960 kS/s
	dst := make([]complex64, 0, f.MaxOutputLen(len(in)))

	b.SetBytes(int64(len(in) * 8))
	for b.Loop() {
		dst = f.Process(dst[:0], in)
	}
}
