package dsp

import (
	"math"
	"testing"
)

// modulatedChannel synthesises what the antenna would actually deliver: an AM
// carrier sitting at offset hertz away from the tuner's centre frequency,
// amplitude-modulated by a single audio tone.
func modulatedChannel(n int, offset, audioFreq, fs float64, amp, depth float64) []complex64 {
	out := make([]complex64, n)
	for i := range out {
		t := float64(i) / fs
		env := amp * (1 + depth*math.Cos(2*math.Pi*audioFreq*t))
		phase := 2 * math.Pi * offset * t
		out[i] = complex(float32(env*math.Cos(phase)), float32(env*math.Sin(phase)))
	}
	return out
}

// toneMagnitude measures the amplitude of a single frequency component,
// which is how the recovered audio is checked against what was transmitted.
func toneMagnitude(x []float32, freq, fs float64) float64 {
	var re, im float64
	for n, v := range x {
		theta := 2 * math.Pi * freq * float64(n) / fs
		re += float64(v) * math.Cos(theta)
		im -= float64(v) * math.Sin(theta)
	}
	return 2 * math.Hypot(re, im) / float64(len(x))
}

// --- decimation planner -----------------------------------------------------

func TestPlanDecimationProducesConsistentStages(t *testing.T) {
	for _, inputRate := range []float64{960_000, 1_024_000, 2_048_000, 2_400_000} {
		plan, err := PlanDecimation(inputRate, 8_000)
		if err != nil {
			t.Fatalf("PlanDecimation(%v): %v", inputRate, err)
		}

		total := 1
		for _, d := range plan.IFDecims {
			if d < 2 || d > MaxStageDecim {
				t.Errorf("%v: stage decimation %d outside [2, %d]", inputRate, d, MaxStageDecim)
			}
			total *= d
		}
		total *= plan.AudioDecim

		if want := int(inputRate / 8_000); total != want {
			t.Errorf("%v: stages multiply to %d, want %d", inputRate, total, want)
		}
		if got := inputRate / float64(product(plan.IFDecims)); got != plan.IFRate {
			t.Errorf("%v: IFRate = %v, but stages give %v", inputRate, plan.IFRate, got)
		}
		if plan.IFRate < MinIFRate {
			t.Errorf("%v: IF rate %v is too low to carry an AM channel", inputRate, plan.IFRate)
		}
	}
}

func product(xs []int) int {
	p := 1
	for _, x := range xs {
		p *= x
	}
	return p
}

// A ratio that is not a whole number cannot be done with integer decimation,
// and must fail here with a clear message rather than somewhere downstream.
func TestPlanDecimationRejectsNonIntegerRatio(t *testing.T) {
	if _, err := PlanDecimation(1_234_567, 8_000); err == nil {
		t.Fatal("expected an error for a non-integer decimation ratio")
	}
}

func TestPlanDecimationRejectsRateBelowAudio(t *testing.T) {
	if _, err := PlanDecimation(4_000, 8_000); err == nil {
		t.Fatal("expected an error when the input rate is below the audio rate")
	}
}

// --- channel chain ----------------------------------------------------------

func newTestChannel(t *testing.T, inputRate float64, maxInput int) *Channel {
	t.Helper()
	ch, err := NewChannel(ChannelOptions{
		Name:            "Tower",
		Offset:          -150_000,
		InputRate:       inputRate,
		AudioRate:       8_000,
		SquelchDB:       -35,
		MaxInputSamples: maxInput,
	})
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}
	return ch
}

// The headline test for the whole phase: transmit a known tone on a known
// channel offset, and require the chain to hand back that tone and nothing else.
func TestChannelRecoversModulatedTone(t *testing.T) {
	const (
		fs        = 960_000.0
		offset    = -150_000.0
		audioFreq = 1_000.0
		audioRate = 8_000.0
	)
	in := modulatedChannel(int(fs/2), offset, audioFreq, fs, 0.2, 0.8) // 500 ms
	ch := newTestChannel(t, fs, len(in))

	audio := append([]float32(nil), ch.Process(in)...)
	if len(audio) == 0 {
		t.Fatal("chain produced no audio")
	}
	if !ch.Open() {
		t.Errorf("squelch stayed closed on a strong carrier (level %.1f dBFS)", ch.LevelDB())
	}

	// Ignore the leading transient while filters fill and the AGC settles.
	settled := audio[len(audio)/2:]

	got := toneMagnitude(settled, audioFreq, audioRate)
	if got < 0.3 || got > 0.7 {
		t.Errorf("recovered tone amplitude %.3f, want ~0.5 after AGC", got)
	}
	for _, spur := range []float64{2_000, 2_500, 3_000} {
		if mag := toneMagnitude(settled, spur, audioRate); mag > got/10 {
			t.Errorf("spurious component at %v Hz: %.4f (tone is %.4f)", spur, mag, got)
		}
	}
}

// The carrier is at -150 kHz, so a chain that ignored the offset, or shifted the
// wrong way, would recover nothing. This pins the sign convention.
func TestChannelIgnoresSignalOnTheWrongOffset(t *testing.T) {
	const fs = 960_000.0
	// Transmit at +150 kHz while the channel is tuned to -150 kHz.
	in := modulatedChannel(int(fs/4), +150_000, 1_000, fs, 0.2, 0.8)
	ch := newTestChannel(t, fs, len(in))

	ch.Process(in)
	if ch.Open() {
		t.Errorf("squelch opened on a signal outside the channel (level %.1f dBFS)", ch.LevelDB())
	}
}

func TestChannelSquelchStaysClosedOnSilence(t *testing.T) {
	const fs = 960_000.0
	in := make([]complex64, int(fs/4))
	ch := newTestChannel(t, fs, len(in))

	audio := ch.Process(in)
	if ch.Open() {
		t.Error("squelch opened on silence")
	}
	for i, v := range audio {
		if v != 0 {
			t.Fatalf("muted channel emitted %v at sample %d", v, i)
		}
	}
}

// A closed squelch must not let the AGC wind up on receiver noise, or the first
// syllable of the next transmission arrives at full scale.
func TestChannelHoldsGainWhileSquelched(t *testing.T) {
	const fs = 960_000.0
	ch := newTestChannel(t, fs, int(fs/10))

	loud := modulatedChannel(int(fs/10), -150_000, 1_000, fs, 0.2, 0.8)
	ch.Process(loud)
	settled := ch.Gain()

	quiet := make([]complex64, int(fs/10))
	for i := range quiet {
		quiet[i] = complex(1e-6, 0)
	}
	for i := 0; i < 5; i++ {
		ch.Process(quiet)
	}

	if math.Abs(float64(ch.Gain()-settled)) > 1e-3 {
		t.Errorf("gain drifted from %v to %v while squelched", settled, ch.Gain())
	}
}

// Real input arrives in whatever block size the driver chooses, which varies.
// The output must not.
func TestChannelChunkingDoesNotChangeResult(t *testing.T) {
	const fs = 960_000.0
	const chunk = 19_200

	in := modulatedChannel(chunk*8, -150_000, 1_000, fs, 0.2, 0.8)

	whole := append([]float32(nil), newTestChannel(t, fs, len(in)).Process(in)...)

	ch := newTestChannel(t, fs, chunk)
	var chunked []float32
	for off := 0; off+chunk <= len(in); off += chunk {
		chunked = append(chunked, ch.Process(in[off:off+chunk])...)
	}

	if len(chunked) != len(whole) {
		t.Fatalf("chunked gave %d audio samples, whole gave %d", len(chunked), len(whole))
	}
	for i := range whole {
		if math.Abs(float64(whole[i]-chunked[i])) > 1e-4 {
			t.Fatalf("audio sample %d differs: whole %v, chunked %v", i, whole[i], chunked[i])
		}
	}
}

func TestChannelRejectsUnplannableRate(t *testing.T) {
	_, err := NewChannel(ChannelOptions{
		Name: "Bad", Offset: 0, InputRate: 1_234_567, AudioRate: 8_000, MaxInputSamples: 1024,
	})
	if err == nil {
		t.Fatal("NewChannel must reject a rate that cannot be decimated to the audio rate")
	}
}

func TestChannelRejectsOffsetBeyondNyquist(t *testing.T) {
	_, err := NewChannel(ChannelOptions{
		Name: "Bad", Offset: 900_000, InputRate: 960_000, AudioRate: 8_000, MaxInputSamples: 1024,
	})
	if err == nil {
		t.Fatal("NewChannel must reject an offset outside the captured band")
	}
}

// The whole point of the fixed buffers: a channel running for days must not
// allocate on the steady-state path.
func TestAllocChannelProcess(t *testing.T) {
	const fs = 960_000.0
	const chunk = 19_200

	ch := newTestChannel(t, fs, chunk)
	in := modulatedChannel(chunk, -150_000, 1_000, fs, 0.2, 0.8)
	ch.Process(in) // warm up

	if n := testing.AllocsPerRun(50, func() { ch.Process(in) }); n != 0 {
		t.Errorf("Channel.Process allocated %v times per run, want 0", n)
	}
}

func BenchmarkChannelProcess(b *testing.B) {
	const fs = 960_000.0
	const chunk = 19_200 // 20 ms

	ch, err := NewChannel(ChannelOptions{
		Name: "Tower", Offset: -150_000, InputRate: fs, AudioRate: 8_000,
		SquelchDB: -35, MaxInputSamples: chunk,
	})
	if err != nil {
		b.Fatal(err)
	}
	in := modulatedChannel(chunk, -150_000, 1_000, fs, 0.2, 0.8)

	b.SetBytes(int64(chunk * 8))
	for b.Loop() {
		ch.Process(in)
	}
}
