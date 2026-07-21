// Package stream turns demodulated audio into formats a listener can consume.
package stream

import (
	"encoding/binary"
	"fmt"
	"io"
)

// WAVHeaderSize is the length of the canonical uncompressed WAV header.
const WAVHeaderSize = 44

// streamingSize is written into the two length fields when the total length
// cannot be known in advance. Zero would be read as an empty file and refused;
// an open-ended value keeps players reading until the connection ends.
const streamingSize = 0xFFFFFFFF

const (
	wavFormatPCM   = 1
	wavChannels    = 1 // airband voice is mono
	wavBitsPerSamp = 16
	wavBytesPerSam = wavBitsPerSamp / 8
)

// WAVWriter writes 16-bit mono PCM in a WAV container.
//
// It serves two rather different purposes with the same code: writing a capture
// to disk for offline listening, and streaming live audio over HTTP to anything
// that can play a URL. The only difference is whether the length fields can be
// filled in afterwards, which is decided by whether the destination can seek.
type WAVWriter struct {
	w    io.Writer
	seek io.WriteSeeker // non-nil when the destination supports rewriting sizes
	rate int

	// scratch is reused so that writing audio allocates nothing.
	scratch []byte
	written uint32
}

// NewWAVWriter writes the header and returns a writer for the audio body. If w
// supports seeking, Close goes back and records the true lengths.
func NewWAVWriter(w io.Writer, sampleRate int) (*WAVWriter, error) {
	if sampleRate <= 0 {
		return nil, fmt.Errorf("sample rate must be positive, got %d", sampleRate)
	}

	ww := &WAVWriter{w: w, rate: sampleRate, scratch: make([]byte, 0, 4096)}
	if s, ok := w.(io.WriteSeeker); ok {
		ww.seek = s
	}

	header := buildWAVHeader(sampleRate, streamingSize)
	if _, err := w.Write(header); err != nil {
		return nil, fmt.Errorf("write wav header: %w", err)
	}
	return ww, nil
}

// buildWAVHeader lays out the 44-byte header for the given data length.
func buildWAVHeader(sampleRate int, dataSize uint32) []byte {
	byteRate := uint32(sampleRate * wavChannels * wavBytesPerSam)
	riffSize := uint32(WAVHeaderSize - 8)
	if dataSize == streamingSize {
		riffSize = streamingSize
	} else {
		riffSize += dataSize
	}

	h := make([]byte, WAVHeaderSize)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], riffSize)
	copy(h[8:], "WAVE")

	copy(h[12:], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16) // PCM fmt chunk length
	binary.LittleEndian.PutUint16(h[20:], wavFormatPCM)
	binary.LittleEndian.PutUint16(h[22:], wavChannels)
	binary.LittleEndian.PutUint32(h[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:], byteRate)
	binary.LittleEndian.PutUint16(h[32:], wavChannels*wavBytesPerSam)
	binary.LittleEndian.PutUint16(h[34:], wavBitsPerSamp)

	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], dataSize)
	return h
}

// Write converts normalised float samples to 16-bit PCM and appends them.
func (w *WAVWriter) Write(samples []float32) error {
	need := len(samples) * wavBytesPerSam
	if cap(w.scratch) < need {
		w.scratch = make([]byte, need)
	}
	buf := w.scratch[:need]

	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*wavBytesPerSam:], uint16(toInt16(s)))
	}
	if _, err := w.w.Write(buf); err != nil {
		return fmt.Errorf("write wav audio: %w", err)
	}

	w.written += uint32(need)
	return nil
}

// toInt16 scales a normalised sample, clipping rather than wrapping. A wrapped
// sample turns a moment of overload into a full-scale click, which is far more
// unpleasant than the clipping it replaces.
func toInt16(s float32) int16 {
	const scale = 32767
	switch {
	case s >= 1:
		return 32767
	case s <= -1:
		return -32768
	}
	return int16(s * scale)
}

// Close records the true lengths when the destination can seek. For a live
// stream there is nothing to correct, so it is a no-op.
func (w *WAVWriter) Close() error {
	if w.seek == nil {
		return nil
	}

	if _, err := w.seek.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind to patch wav header: %w", err)
	}
	if _, err := w.seek.Write(buildWAVHeader(w.rate, w.written)); err != nil {
		return fmt.Errorf("patch wav header: %w", err)
	}
	if _, err := w.seek.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("return to end of wav: %w", err)
	}
	return nil
}
