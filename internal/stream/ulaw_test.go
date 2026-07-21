package stream

import (
	"math"
	"testing"
)

// G.711 fixes these three encodings, and every player and phone network agrees
// on them. Pinning them here means an implementation mistake shows up as a
// wrong constant rather than as audio that is quietly distorted.
func TestULawReferenceValues(t *testing.T) {
	cases := map[string]struct {
		pcm  int16
		ulaw byte
	}{
		"silence":      {0, 0xFF},
		"positive max": {32635, 0x80},
		"negative max": {-32635, 0x00},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := EncodeULaw(c.pcm); got != c.ulaw {
				t.Errorf("EncodeULaw(%d) = %#02x, want %#02x", c.pcm, got, c.ulaw)
			}
		})
	}
}

func TestULawSilenceDecodesToZero(t *testing.T) {
	if got := DecodeULaw(0xFF); got != 0 {
		t.Errorf("DecodeULaw(0xFF) = %d, want 0", got)
	}
}

// mu-law is lossy by design: it trades absolute accuracy for a consistent
// signal-to-noise ratio across a wide dynamic range, which is exactly what
// speech needs. The error should stay proportional to the sample.
func TestULawRoundTripStaysProportional(t *testing.T) {
	for _, pcm := range []int16{0, 1, 100, 1000, 5000, 16000, 32000, -100, -5000, -32000} {
		got := DecodeULaw(EncodeULaw(pcm))

		tolerance := math.Max(float64(abs16(pcm))*0.08, 40)
		if diff := math.Abs(float64(got) - float64(pcm)); diff > tolerance {
			t.Errorf("round trip of %d gave %d (error %.0f, tolerance %.0f)",
				pcm, got, diff, tolerance)
		}
	}
}

func TestULawIsMonotonic(t *testing.T) {
	prev := DecodeULaw(EncodeULaw(-32000))
	for pcm := -31000; pcm <= 32000; pcm += 250 {
		got := DecodeULaw(EncodeULaw(int16(pcm)))
		if got < prev {
			t.Fatalf("encoding is not monotonic: %d decoded to %d after %d", pcm, got, prev)
		}
		prev = got
	}
}

func TestULawClipsRatherThanWraps(t *testing.T) {
	// Values beyond the encodable range must saturate, not fold over to the
	// opposite sign, which would be an audible crack.
	if got := EncodeULaw(32767); got != 0x80 {
		t.Errorf("EncodeULaw(32767) = %#02x, want the positive maximum 0x80", got)
	}
	if got := EncodeULaw(-32768); got != 0x00 {
		t.Errorf("EncodeULaw(-32768) = %#02x, want the negative maximum 0x00", got)
	}
}

// --- block encoding ---------------------------------------------------------

func TestEncodeULawBlockConvertsNormalisedFloats(t *testing.T) {
	in := []float32{0, 1, -1}
	dst := make([]byte, len(in))

	if n := EncodeULawBlock(dst, in); n != len(in) {
		t.Fatalf("encoded %d samples, want %d", n, len(in))
	}
	if dst[0] != 0xFF {
		t.Errorf("0.0 encoded to %#02x, want silence 0xFF", dst[0])
	}
	if dst[1] != 0x80 {
		t.Errorf("+1.0 encoded to %#02x, want positive max 0x80", dst[1])
	}
	if dst[2] != 0x00 {
		t.Errorf("-1.0 encoded to %#02x, want negative max 0x00", dst[2])
	}
}

func TestEncodeULawBlockRespectsShortDestination(t *testing.T) {
	in := make([]float32, 10)
	dst := make([]byte, 4)
	if n := EncodeULawBlock(dst, in); n != 4 {
		t.Errorf("encoded %d samples into a 4-byte buffer, want 4", n)
	}
}

// A tone must survive the round trip recognisably: this is the end-to-end check
// that the codec is usable for speech, not just self-consistent.
func TestULawPreservesAToneWithUsableSNR(t *testing.T) {
	const n = 8000
	in := make([]float32, n)
	for i := range in {
		in[i] = 0.5 * float32(math.Sin(2*math.Pi*1000*float64(i)/8000))
	}

	encoded := make([]byte, n)
	EncodeULawBlock(encoded, in)

	var signal, noise float64
	for i, b := range encoded {
		got := float64(DecodeULaw(b)) / 32768
		signal += float64(in[i]) * float64(in[i])
		noise += (got - float64(in[i])) * (got - float64(in[i]))
	}

	snr := 10 * math.Log10(signal/noise)
	if snr < 30 {
		t.Errorf("round-trip SNR is %.1f dB, want at least 30 dB for speech", snr)
	}
}

func TestAllocEncodeULawBlock(t *testing.T) {
	in := make([]float32, 480)
	dst := make([]byte, 480)
	if n := testing.AllocsPerRun(100, func() {
		EncodeULawBlock(dst, in)
	}); n != 0 {
		t.Errorf("EncodeULawBlock allocated %v times per run, want 0", n)
	}
}

func abs16(v int16) int16 {
	if v < 0 {
		return -v
	}
	return v
}
