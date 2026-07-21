package dsp

import (
	"math"
	"testing"
)

// tone generates a complex exponential at freq, the cleanest possible test
// signal: mixing it by -freq must collapse it to a constant.
func tone(n int, freq, fs float64) []complex64 {
	out := make([]complex64, n)
	for i := range out {
		phase := 2 * math.Pi * freq * float64(i) / fs
		out[i] = complex(float32(math.Cos(phase)), float32(math.Sin(phase)))
	}
	return out
}

func TestNCOShiftsToneToDC(t *testing.T) {
	const fs, freq = 960_000.0, -150_000.0

	// A channel 150 kHz below centre must land at DC after the shift.
	in := tone(4096, freq, fs)
	got := NewNCO(-freq, fs).Mix(make([]complex64, 0, len(in)), in)

	if len(got) != len(in) {
		t.Fatalf("got %d samples, want %d", len(got), len(in))
	}
	// Every output must equal the first: a tone at DC is a constant phasor.
	for i, v := range got {
		if !closeC(v, got[0], 1e-4) {
			t.Fatalf("sample %d = %v, want constant %v; tone did not land at DC", i, v, got[0])
		}
	}
}

func TestNCOZeroShiftIsIdentity(t *testing.T) {
	in := tone(512, 12_000, 96_000)
	got := NewNCO(0, 96_000).Mix(make([]complex64, 0, len(in)), in)
	for i := range in {
		if !closeC(got[i], in[i], 1e-6) {
			t.Fatalf("sample %d = %v, want %v", i, got[i], in[i])
		}
	}
}

// The phasor is advanced by repeated complex multiplication, which drifts off
// the unit circle over time. Without periodic renormalisation the signal would
// slowly gain or lose amplitude over hours of running.
func TestNCOPreservesMagnitudeOverLongRun(t *testing.T) {
	const fs = 960_000.0
	n := 2_000_000 // a couple of seconds at the edge sample rate

	in := make([]complex64, n)
	for i := range in {
		in[i] = complex(1, 0)
	}
	got := NewNCO(-150_000, fs).Mix(make([]complex64, 0, n), in)

	for _, i := range []int{0, n / 2, n - 1} {
		mag := math.Hypot(float64(real(got[i])), float64(imag(got[i])))
		if math.Abs(mag-1) > 1e-4 {
			t.Errorf("magnitude at sample %d = %.9f, want 1; phasor drifted", i, mag)
		}
	}
}

func TestNCOChunkingDoesNotChangeResult(t *testing.T) {
	const fs = 960_000.0
	in := tone(1000, 50_000, fs)

	whole := NewNCO(-150_000, fs).Mix(make([]complex64, 0, len(in)), in)

	n := NewNCO(-150_000, fs)
	chunked := make([]complex64, 0, len(in))
	rest := in
	for _, size := range []int{1, 7, 64, 3, 200, 725} {
		chunked = n.Mix(chunked, rest[:size])
		rest = rest[size:]
	}

	if len(chunked) != len(whole) {
		t.Fatalf("chunked gave %d samples, whole gave %d", len(chunked), len(whole))
	}
	for i := range whole {
		if !closeC(whole[i], chunked[i], 1e-5) {
			t.Fatalf("sample %d differs: whole %v, chunked %v", i, whole[i], chunked[i])
		}
	}
}

func TestNCOResetReturnsToInitialPhase(t *testing.T) {
	in := tone(256, 20_000, 960_000)
	n := NewNCO(-150_000, 960_000)

	first := append([]complex64(nil), n.Mix(make([]complex64, 0, 256), in)...)
	n.Reset()
	second := n.Mix(make([]complex64, 0, 256), in)

	for i := range first {
		if !closeC(first[i], second[i], 1e-6) {
			t.Fatalf("after Reset, sample %d = %v, want %v", i, second[i], first[i])
		}
	}
}

func TestAllocNCO(t *testing.T) {
	n := NewNCO(-150_000, 960_000)
	in := make([]complex64, 1024)
	dst := make([]complex64, 0, len(in))

	if got := testing.AllocsPerRun(100, func() {
		dst = n.Mix(dst[:0], in)
	}); got != 0 {
		t.Errorf("NCO.Mix allocated %v times per run, want 0", got)
	}
}

func BenchmarkNCOMix(b *testing.B) {
	n := NewNCO(-150_000, 960_000)
	in := make([]complex64, 19_200) // 20 ms at 960 kS/s
	dst := make([]complex64, 0, len(in))

	b.SetBytes(int64(len(in) * 8))
	for b.Loop() {
		dst = n.Mix(dst[:0], in)
	}
}
