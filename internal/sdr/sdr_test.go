package sdr

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ramp builds a recognisable sample pattern: real part counts up, imaginary
// part counts down, so any offset or ordering mistake is obvious in a failure.
func ramp(n int) []complex64 {
	out := make([]complex64, n)
	for i := range out {
		out[i] = complex(float32(i), float32(-i))
	}
	return out
}

func writeCF32(t *testing.T, samples []complex64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capture.cf32")
	buf := make([]byte, len(samples)*SampleBytes)
	for i, s := range samples {
		binary.LittleEndian.PutUint32(buf[i*SampleBytes:], math.Float32bits(real(s)))
		binary.LittleEndian.PutUint32(buf[i*SampleBytes+4:], math.Float32bits(imag(s)))
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// drain collects up to want samples, releasing every block back to the pool.
func drain(t *testing.T, ch <-chan *Block, want int) []complex64 {
	t.Helper()
	var got []complex64
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case b, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, b.Samples...)
			b.Release()
		case <-deadline:
			t.Fatalf("timed out with %d/%d samples", len(got), want)
		}
	}
	return got
}

// --- cf32 codec -------------------------------------------------------------

func TestCF32RoundTrip(t *testing.T) {
	in := ramp(64)
	buf := make([]byte, len(in)*SampleBytes)
	if n := EncodeCF32(buf, in); n != len(in) {
		t.Fatalf("encoded %d samples, want %d", n, len(in))
	}

	out := make([]complex64, len(in))
	if n := DecodeCF32(out, buf); n != len(in) {
		t.Fatalf("decoded %d samples, want %d", n, len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("sample %d = %v, want %v", i, out[i], in[i])
		}
	}
}

// cf32 is interleaved little-endian float32, which is what rx_sdr and GNU Radio
// write. Getting the byte order wrong would still round-trip through our own
// code, so it is pinned explicitly.
func TestCF32IsLittleEndianInterleaved(t *testing.T) {
	buf := make([]byte, SampleBytes)
	EncodeCF32(buf, []complex64{complex(1, -1)})

	if got := math.Float32frombits(binary.LittleEndian.Uint32(buf[0:])); got != 1 {
		t.Errorf("real part = %v, want 1", got)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(buf[4:])); got != -1 {
		t.Errorf("imag part = %v, want -1", got)
	}
}

func TestCF32HandlesPartialBuffers(t *testing.T) {
	in := ramp(8)
	buf := make([]byte, 4*SampleBytes) // room for half
	if n := EncodeCF32(buf, in); n != 4 {
		t.Errorf("encode into short buffer wrote %d samples, want 4", n)
	}

	out := make([]complex64, 8)
	// A trailing partial sample must be ignored, not misread.
	if n := DecodeCF32(out, buf[:3*SampleBytes+5]); n != 3 {
		t.Errorf("decode of 3.5 samples returned %d, want 3", n)
	}
}

func TestAllocCF32Codec(t *testing.T) {
	in := ramp(1024)
	buf := make([]byte, len(in)*SampleBytes)
	out := make([]complex64, len(in))

	if n := testing.AllocsPerRun(100, func() {
		EncodeCF32(buf, in)
		DecodeCF32(out, buf)
	}); n != 0 {
		t.Errorf("cf32 codec allocated %v times per run, want 0", n)
	}
}

// --- block pool -------------------------------------------------------------

func TestBlockPoolHandsOutPreallocatedBlocks(t *testing.T) {
	p := NewBlockPool(2, 128)

	b, ok := p.Get()
	if !ok {
		t.Fatal("Get on a fresh pool must succeed")
	}
	if len(b.Samples) != 128 {
		t.Errorf("block holds %d samples, want 128", len(b.Samples))
	}
	if p.Free() != 1 {
		t.Errorf("Free() = %d after one Get, want 1", p.Free())
	}
}

// The pool is fixed-size by design: it is what stops a stalled consumer from
// growing the heap without bound on a 512 MB device. Exhaustion must be visible
// to the caller so it can drop, not block.
func TestBlockPoolReportsExhaustion(t *testing.T) {
	p := NewBlockPool(2, 16)
	first, _ := p.Get()
	if _, ok := p.Get(); !ok {
		t.Fatal("second Get must succeed")
	}
	if _, ok := p.Get(); ok {
		t.Fatal("third Get must report exhaustion rather than allocate")
	}

	first.Release()
	if _, ok := p.Get(); !ok {
		t.Fatal("Get must succeed after a Release")
	}
}

func TestBlockReleaseResetsState(t *testing.T) {
	p := NewBlockPool(1, 16)
	b, _ := p.Get()
	b.Overflow = true
	b.Samples = b.Samples[:4]
	b.Release()

	reused, _ := p.Get()
	if reused.Overflow {
		t.Error("Overflow must be cleared on reuse")
	}
	if len(reused.Samples) != 16 {
		t.Errorf("reused block has %d samples, want full length 16", len(reused.Samples))
	}
}

func TestBlockDoubleReleaseIsNotFatal(t *testing.T) {
	p := NewBlockPool(1, 16)
	b, _ := p.Get()
	b.Release()
	b.Release() // must not corrupt the pool or panic

	if free := p.Free(); free != 1 {
		t.Errorf("Free() = %d after a double release, want 1", free)
	}
}

func TestAllocBlockPool(t *testing.T) {
	p := NewBlockPool(4, 1024)
	if n := testing.AllocsPerRun(100, func() {
		b, ok := p.Get()
		if !ok {
			t.Fatal("pool exhausted during allocation test")
		}
		b.Release()
	}); n != 0 {
		t.Errorf("pool Get/Release allocated %v times per run, want 0", n)
	}
}

// --- file source ------------------------------------------------------------

func newFileSource(t *testing.T, path string, opts func(*FileOptions)) *FileSource {
	t.Helper()
	o := FileOptions{
		Path:       path,
		SampleRate: 960_000,
		CenterFreq: 118_250_000,
		BlockSize:  16,
		Realtime:   false, // tests replay as fast as possible
	}
	if opts != nil {
		opts(&o)
	}
	src, err := NewFileSource(o)
	if err != nil {
		t.Fatalf("NewFileSource: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

func TestFileSourceReplaysSamplesInOrder(t *testing.T) {
	want := ramp(64)
	src := newFileSource(t, writeCF32(t, want), nil)

	ch, err := src.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := drain(t, ch, len(want))
	if len(got) < len(want) {
		t.Fatalf("got %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFileSourceClosesChannelAtEOFWhenNotLooping(t *testing.T) {
	src := newFileSource(t, writeCF32(t, ramp(32)), func(o *FileOptions) { o.Loop = false })
	ch, err := src.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	for range ch { //nolint:revive // draining until close is the assertion
	}
	// Reaching here means the channel closed; a leaked goroutine would hang.
}

func TestFileSourceLoopsAtEOF(t *testing.T) {
	want := ramp(32)
	src := newFileSource(t, writeCF32(t, want), func(o *FileOptions) { o.Loop = true })
	ch, err := src.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := drain(t, ch, len(want)*3)
	if len(got) < len(want)*3 {
		t.Fatalf("got %d samples, want %d", len(got), len(want)*3)
	}
	for i := range got {
		if got[i] != want[i%len(want)] {
			t.Fatalf("sample %d = %v, want %v (loop position %d)",
				i, got[i], want[i%len(want)], i%len(want))
		}
	}
}

func TestFileSourceStopsOnContextCancel(t *testing.T) {
	src := newFileSource(t, writeCF32(t, ramp(4096)), func(o *FileOptions) { o.Loop = true })
	ctx, cancel := context.WithCancel(t.Context())
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	<-ch
	cancel()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case b, ok := <-ch:
			if !ok {
				return // closed as required
			}
			b.Release()
		case <-deadline:
			t.Fatal("source did not stop within 5s of context cancellation")
		}
	}
}

func TestFileSourceRejectsMissingFile(t *testing.T) {
	_, err := NewFileSource(FileOptions{
		Path: filepath.Join(t.TempDir(), "absent.cf32"), SampleRate: 960_000,
	})
	if err == nil {
		t.Fatal("NewFileSource must fail on a missing file")
	}
}

func TestFileSourceRejectsInvalidSampleRate(t *testing.T) {
	_, err := NewFileSource(FileOptions{Path: writeCF32(t, ramp(8)), SampleRate: 0})
	if err == nil {
		t.Fatal("NewFileSource must reject a zero sample rate")
	}
}

func TestFileSourceReportsItsConfiguration(t *testing.T) {
	src := newFileSource(t, writeCF32(t, ramp(8)), nil)
	if src.SampleRate() != 960_000 {
		t.Errorf("SampleRate() = %v", src.SampleRate())
	}
	if src.CenterFreq() != 118_250_000 {
		t.Errorf("CenterFreq() = %v", src.CenterFreq())
	}
	if src.Describe() == "" {
		t.Error("Describe() must not be empty")
	}
}

// FileSource must satisfy Source; this fails at compile time if it drifts.
var _ Source = (*FileSource)(nil)
