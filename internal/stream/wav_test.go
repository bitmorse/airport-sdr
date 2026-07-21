package stream

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func u16(t *testing.T, b []byte, off int) uint16 {
	t.Helper()
	return binary.LittleEndian.Uint16(b[off:])
}

func u32(t *testing.T, b []byte, off int) uint32 {
	t.Helper()
	return binary.LittleEndian.Uint32(b[off:])
}

func TestWAVHeaderLayout(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewWAVWriter(&buf, 8_000); err != nil {
		t.Fatalf("NewWAVWriter: %v", err)
	}
	h := buf.Bytes()

	if len(h) != WAVHeaderSize {
		t.Fatalf("header is %d bytes, want %d", len(h), WAVHeaderSize)
	}
	for _, c := range []struct {
		off  int
		want string
	}{{0, "RIFF"}, {8, "WAVE"}, {12, "fmt "}, {36, "data"}} {
		if got := string(h[c.off : c.off+4]); got != c.want {
			t.Errorf("offset %d = %q, want %q", c.off, got, c.want)
		}
	}

	if got := u32(t, h, 16); got != 16 {
		t.Errorf("fmt chunk size = %d, want 16 (PCM)", got)
	}
	if got := u16(t, h, 20); got != 1 {
		t.Errorf("audio format = %d, want 1 (uncompressed PCM)", got)
	}
	if got := u16(t, h, 22); got != 1 {
		t.Errorf("channels = %d, want 1 (mono)", got)
	}
	if got := u32(t, h, 24); got != 8_000 {
		t.Errorf("sample rate = %d, want 8000", got)
	}
	// 8000 Hz * 1 channel * 2 bytes
	if got := u32(t, h, 28); got != 16_000 {
		t.Errorf("byte rate = %d, want 16000", got)
	}
	if got := u16(t, h, 32); got != 2 {
		t.Errorf("block align = %d, want 2", got)
	}
	if got := u16(t, h, 34); got != 16 {
		t.Errorf("bits per sample = %d, want 16", got)
	}
}

// A player reading from a live stream cannot be told the length up front, so
// the size fields are left wide open rather than zero, which most players
// interpret as an empty file and refuse to play.
func TestWAVStreamingHeaderHasOpenEndedSizes(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewWAVWriter(&buf, 8_000); err != nil {
		t.Fatal(err)
	}
	h := buf.Bytes()

	if got := u32(t, h, 4); got != streamingSize {
		t.Errorf("RIFF size = %d, want the open-ended placeholder", got)
	}
	if got := u32(t, h, 40); got != streamingSize {
		t.Errorf("data size = %d, want the open-ended placeholder", got)
	}
}

func TestWAVConvertsFloatToInt16(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWAVWriter(&buf, 8_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write([]float32{0, 1, -1, 0.5}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	body := buf.Bytes()[WAVHeaderSize:]
	want := []int16{0, 32767, -32768, 16383}
	if len(body) != len(want)*2 {
		t.Fatalf("wrote %d bytes, want %d", len(body), len(want)*2)
	}
	for i, w := range want {
		if got := int16(u16(t, body, i*2)); got != w {
			t.Errorf("sample %d = %d, want %d", i, got, w)
		}
	}
}

// The DSP chain limits its output, but a bug upstream must not wrap a loud
// sample around to full-scale negative, which is audible as a vicious click.
func TestWAVClipsRatherThanWraps(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWAVWriter(&buf, 8_000)
	if err := w.Write([]float32{9, -9}); err != nil {
		t.Fatal(err)
	}

	body := buf.Bytes()[WAVHeaderSize:]
	if got := int16(u16(t, body, 0)); got != 32767 {
		t.Errorf("sample above full scale = %d, want 32767", got)
	}
	if got := int16(u16(t, body, 2)); got != -32768 {
		t.Errorf("sample below full scale = %d, want -32768", got)
	}
}

// Writing to a file, unlike a stream, can go back and record the true length,
// which is what makes the result seekable in an audio editor.
func TestWAVFileGetsRealSizesOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWAVWriter(f, 8_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(make([]float32, 100)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.Close()

	h, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := u32(t, h, 40); got != 200 {
		t.Errorf("data size = %d, want 200 bytes for 100 samples", got)
	}
	if got := u32(t, h, 4); got != 236 {
		t.Errorf("RIFF size = %d, want 236 (36 + 200)", got)
	}
}

func TestWAVRejectsBadSampleRate(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewWAVWriter(&buf, 0); err == nil {
		t.Fatal("expected an error for a zero sample rate")
	}
}

func TestAllocWAVWrite(t *testing.T) {
	w, _ := NewWAVWriter(discard{}, 8_000)
	samples := make([]float32, 480)

	if n := testing.AllocsPerRun(100, func() {
		_ = w.Write(samples)
	}); n != 0 {
		t.Errorf("WAVWriter.Write allocated %v times per run, want 0", n)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
