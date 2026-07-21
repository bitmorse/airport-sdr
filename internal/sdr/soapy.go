//go:build soapy

package sdr

/*
#cgo LDFLAGS: -lSoapySDR
#include <SoapySDR/Device.h>
#include <SoapySDR/Formats.h>
#include <SoapySDR/Errors.h>
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// This file is the only place in the program that talks to C, and it is kept
// deliberately dumb. Every decision about what settings are acceptable lives in
// tune.go, which is pure Go and fully tested; the code here just translates.
// That split is what makes a layer that cannot be unit-tested small enough to
// review by eye.

const (
	readTimeoutUS = 200_000 // 200 ms; long enough to be patient, short enough to notice cancellation
	// maxConsecutiveErrors is how many failed reads are tolerated before the
	// device is considered lost and a reconnect is attempted.
	maxConsecutiveErrors = 10
	reconnectBackoffMin  = 500 * time.Millisecond
	reconnectBackoffMax  = 30 * time.Second
	// sampleRateTolerance is how far the device's actual rate may differ from
	// what was asked before we refuse to run.
	sampleRateTolerance = 1.0 // hertz
)

// SoapySource streams IQ from any SoapySDR-supported radio.
type SoapySource struct {
	opts SoapyOptions
	res  Resolution
	caps DeviceCaps
	desc string

	dev    *C.SoapySDRDevice
	stream *C.SoapySDRStream

	// The receive buffer lives in C memory. Handing readStream a Go pointer
	// would mean passing C a pointer to memory containing another Go pointer,
	// which the cgo pointer rules forbid; copying out of a C buffer costs one
	// memcpy per block and sidesteps the problem entirely.
	cbuf  unsafe.Pointer
	buffs *unsafe.Pointer

	pool      *BlockPool
	blockSize int

	overflows atomic.Uint64
	closeOnce sync.Once
	mu        sync.Mutex // guards dev and stream during reconnect
}

var soapyRX = C.int(C.SOAPY_SDR_RX)

// lastError reports the driver's own description of the most recent failure,
// which is usually far more specific than the numeric code.
func lastError() string {
	if msg := C.GoString(C.SoapySDRDevice_lastError()); msg != "" {
		return msg
	}
	return "no further detail from the driver"
}

// NewSoapySource opens a device, validates the requested settings against what
// it reports supporting, and applies them.
func NewSoapySource(opts SoapyOptions) (Source, error) {
	if opts.BlockSize <= 0 {
		opts.BlockSize = BlockSizeFor(opts.SampleRate)
	}
	if opts.PoolSize <= 0 {
		opts.PoolSize = defaultPoolBlocks
	}

	s := &SoapySource{opts: opts}
	if err := s.open(); err != nil {
		return nil, err
	}
	s.allocBuffers()

	return s, nil
}

// allocBuffers sizes the block pool and the C receive buffer for the current
// block size, releasing anything previously allocated. Callers hold mu.
func (s *SoapySource) allocBuffers() {
	s.freeBuffers()

	s.blockSize = s.opts.BlockSize
	s.pool = NewBlockPool(s.opts.PoolSize, s.opts.BlockSize)
	s.cbuf = C.malloc(C.size_t(s.opts.BlockSize * SampleBytes))
	s.buffs = (*unsafe.Pointer)(C.malloc(C.size_t(unsafe.Sizeof(uintptr(0)))))
	*s.buffs = s.cbuf
}

func (s *SoapySource) freeBuffers() {
	if s.buffs != nil {
		C.free(unsafe.Pointer(s.buffs))
		s.buffs = nil
	}
	if s.cbuf != nil {
		C.free(s.cbuf)
		s.cbuf = nil
	}
}

// Retune moves the device to a new group's tuning.
//
// The device is closed and reopened rather than adjusted in place: the sample
// rate may change, which resizes the stream and the buffers behind it, and the
// reopen path is the one already proven by reconnect(). Reopening also puts the
// request back through Resolve, so an unsupported rate is refused rather than
// silently snapped to something close.
//
// Must not be called while a stream is running; see the Source interface.
func (s *SoapySource) Retune(centerFreq, sampleRate float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous := s.opts
	s.opts.CenterFreq = centerFreq
	s.opts.SampleRate = sampleRate
	s.opts.BlockSize = BlockSizeFor(sampleRate)

	s.closeDevice()
	if err := s.open(); err != nil {
		// Put the device back where it was, so a rejected retune costs a gap in
		// the audio rather than a dead receiver.
		s.opts = previous
		s.closeDevice()
		if restoreErr := s.open(); restoreErr != nil {
			return fmt.Errorf("retune to %.4f MHz at %.3f MS/s failed (%w), and "+
				"restoring the previous tuning also failed: %v",
				centerFreq/1e6, sampleRate/1e6, err, restoreErr)
		}
		return fmt.Errorf("retune to %.4f MHz at %.3f MS/s: %w",
			centerFreq/1e6, sampleRate/1e6, err)
	}

	s.allocBuffers()
	return nil
}

// ProbeDevice opens a device, reports what it says it supports, and closes it
// again without changing any settings.
//
// This exists because "the receiver is deaf" and "the channel is quiet" look
// identical from the audio, and the usual cause is an antenna port that is
// wrong for the band. Asking the device what ports it has beats guessing.
func ProbeDevice(deviceArgs string) (DeviceCaps, error) {
	args := C.CString(deviceArgs)
	defer C.free(unsafe.Pointer(args))

	dev := C.SoapySDRDevice_makeStrArgs(args)
	if dev == nil {
		return DeviceCaps{}, fmt.Errorf("open SDR (args %q): %s", deviceArgs, lastError())
	}
	defer C.SoapySDRDevice_unmake(dev) //nolint:errcheck // nothing to do if closing fails

	return queryCaps(dev), nil
}

// open makes the device, checks the request against its capabilities, applies
// the settings and sets up the stream.
func (s *SoapySource) open() error {
	args := C.CString(s.opts.DeviceArgs)
	defer C.free(unsafe.Pointer(args))

	dev := C.SoapySDRDevice_makeStrArgs(args)
	if dev == nil {
		return fmt.Errorf("open SDR (args %q): %s", s.opts.DeviceArgs, lastError())
	}
	s.dev = dev
	s.caps = queryCaps(dev)

	res, err := Resolve(TuneRequest{
		SampleRate: s.opts.SampleRate,
		CenterFreq: s.opts.CenterFreq,
		Gain:       s.opts.Gain,
		AutoGain:   s.opts.AutoGain,
		Antenna:    s.opts.Antenna,
	}, s.caps)
	if err != nil {
		s.closeDevice()
		return fmt.Errorf("device cannot be configured as requested: %w", err)
	}
	s.res = res
	for _, note := range res.Notes {
		slog.Warn("sdr setting adjusted", "note", note)
	}

	if err := s.applySettings(); err != nil {
		s.closeDevice()
		return err
	}
	return s.openStream()
}

// applySettings pushes the resolved configuration to the device and reads it
// back. A driver that quietly substitutes a different sample rate would break
// the decimation plan, so the value is verified rather than trusted.
func (s *SoapySource) applySettings() error {
	if ret := C.SoapySDRDevice_setSampleRate(s.dev, soapyRX, 0, C.double(s.res.SampleRate)); ret != 0 {
		return fmt.Errorf("set sample rate %v: %s", s.res.SampleRate, lastError())
	}
	actual := float64(C.SoapySDRDevice_getSampleRate(s.dev, soapyRX, 0))
	if math.Abs(actual-s.res.SampleRate) > sampleRateTolerance {
		return fmt.Errorf(
			"device applied a sample rate of %v instead of the requested %v; "+
				"the decimation plan requires an exact rate", actual, s.res.SampleRate)
	}

	if ret := C.SoapySDRDevice_setFrequency(
		s.dev, soapyRX, 0, C.double(s.res.CenterFreq), nil); ret != 0 {
		return fmt.Errorf("set centre frequency %v: %s", s.res.CenterFreq, lastError())
	}

	if err := s.applyGain(); err != nil {
		return err
	}
	if s.res.Antenna != "" {
		name := C.CString(s.res.Antenna)
		defer C.free(unsafe.Pointer(name))
		if ret := C.SoapySDRDevice_setAntenna(s.dev, soapyRX, 0, name); ret != 0 {
			return fmt.Errorf("select antenna %q: %s", s.res.Antenna, lastError())
		}
	}
	if s.opts.PPM != 0 {
		// Not every driver supports correction; a refusal is worth a warning
		// but is not fatal.
		if ret := C.SoapySDRDevice_setFrequencyCorrection(
			s.dev, soapyRX, 0, C.double(s.opts.PPM)); ret != 0 {
			slog.Warn("device rejected ppm correction", "ppm", s.opts.PPM, "err", lastError())
		}
	}

	s.desc = fmt.Sprintf("soapy %q @ %.3f MS/s, centre %.4f MHz",
		s.opts.DeviceArgs, s.res.SampleRate/1e6, s.res.CenterFreq/1e6)
	return nil
}

func (s *SoapySource) applyGain() error {
	if s.res.AutoGain {
		if ret := C.SoapySDRDevice_setGainMode(s.dev, soapyRX, 0, C.bool(true)); ret != 0 {
			return fmt.Errorf("enable automatic gain: %s", lastError())
		}
		return nil
	}
	// Switching hardware AGC off is best-effort: some drivers have no such mode.
	C.SoapySDRDevice_setGainMode(s.dev, soapyRX, 0, C.bool(false)) //nolint:errcheck // optional
	if ret := C.SoapySDRDevice_setGain(s.dev, soapyRX, 0, C.double(s.res.Gain)); ret != 0 {
		return fmt.Errorf("set gain %v: %s", s.res.Gain, lastError())
	}
	return nil
}

func (s *SoapySource) openStream() error {
	format := C.CString("CF32")
	defer C.free(unsafe.Pointer(format))

	channels := [1]C.size_t{0}
	stream := C.SoapySDRDevice_setupStream(s.dev, soapyRX, format, &channels[0], 1, nil)
	if stream == nil {
		return fmt.Errorf("set up RX stream: %s", lastError())
	}
	s.stream = stream

	if ret := C.SoapySDRDevice_activateStream(s.dev, s.stream, 0, 0, 0); ret != 0 {
		return fmt.Errorf("activate RX stream: %s", lastError())
	}
	return nil
}

// queryCaps asks the device what it supports. Anything it declines to report
// comes back empty, and Resolve treats that as a reason to refuse rather than
// as permission to guess.
func queryCaps(dev *C.SoapySDRDevice) DeviceCaps {
	var caps DeviceCaps

	var n C.size_t
	caps.SampleRates = rangesFrom(C.SoapySDRDevice_getSampleRateRange(dev, soapyRX, 0, &n), n)
	caps.Frequencies = rangesFrom(C.SoapySDRDevice_getFrequencyRange(dev, soapyRX, 0, &n), n)

	gr := C.SoapySDRDevice_getGainRange(dev, soapyRX, 0)
	caps.Gains = Range{Min: float64(gr.minimum), Max: float64(gr.maximum), Step: float64(gr.step)}

	if arr := C.SoapySDRDevice_listAntennas(dev, soapyRX, 0, &n); arr != nil && n > 0 {
		for _, str := range unsafe.Slice(arr, int(n)) {
			caps.Antennas = append(caps.Antennas, C.GoString(str))
		}
		C.SoapySDRStrings_clear(&arr, n)
	}
	return caps
}

// rangesFrom converts a malloc'd C array of ranges and frees it.
func rangesFrom(ptr *C.SoapySDRRange, n C.size_t) []Range {
	if ptr == nil || n == 0 {
		return nil
	}
	defer C.free(unsafe.Pointer(ptr))

	out := make([]Range, 0, int(n))
	for _, r := range unsafe.Slice(ptr, int(n)) {
		out = append(out, Range{
			Min: float64(r.minimum), Max: float64(r.maximum), Step: float64(r.step),
		})
	}
	return out
}

func (s *SoapySource) SampleRate() float64 { return s.res.SampleRate }
func (s *SoapySource) CenterFreq() float64 { return s.res.CenterFreq }
func (s *SoapySource) Describe() string    { return s.desc }

// Capabilities reports what the device said it supports, for diagnostics.
func (s *SoapySource) Capabilities() DeviceCaps { return s.caps }

// Overflows counts blocks during which the device dropped samples, almost
// always because the host could not keep up with the USB stream.
func (s *SoapySource) Overflows() uint64 { return s.overflows.Load() }

func (s *SoapySource) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closeDevice()
		s.freeBuffers()
	})
	return nil
}

// closeDevice tears down the stream and device. Callers hold mu.
func (s *SoapySource) closeDevice() {
	if s.stream != nil {
		C.SoapySDRDevice_deactivateStream(s.dev, s.stream, 0, 0) //nolint:errcheck // shutting down
		C.SoapySDRDevice_closeStream(s.dev, s.stream)            //nolint:errcheck // shutting down
		s.stream = nil
	}
	if s.dev != nil {
		C.SoapySDRDevice_unmake(s.dev) //nolint:errcheck // shutting down
		s.dev = nil
	}
}

// Start streams until the context ends or the device cannot be recovered.
func (s *SoapySource) Start(ctx context.Context) (<-chan *Block, error) {
	out := make(chan *Block)
	go s.run(ctx, out)
	return out, nil
}

// run is the receive event loop. Power of 10 rule 2 asks for bounded loops;
// this is the documented exception, and it terminates on context cancellation.
func (s *SoapySource) run(ctx context.Context, out chan<- *Block) {
	defer close(out)

	backoff := reconnectBackoffMin
	for ctx.Err() == nil {
		err := s.receive(ctx, out)
		if err == nil || ctx.Err() != nil {
			return
		}

		slog.Error("sdr stream failed, attempting to reconnect",
			"err", err, "retry_in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}

		if err := s.reconnect(); err != nil {
			slog.Error("sdr reconnect failed", "err", err)
			if backoff *= 2; backoff > reconnectBackoffMax {
				backoff = reconnectBackoffMax
			}
			continue
		}
		slog.Info("sdr reconnected", "device", s.desc)
		backoff = reconnectBackoffMin
	}
}

// receive reads blocks until the context ends or the device stops responding.
func (s *SoapySource) receive(ctx context.Context, out chan<- *Block) error {
	consecutive := 0

	for ctx.Err() == nil {
		block, ok := s.pool.Get()
		if !ok {
			// Every block is still with a consumer. Dropping here is correct:
			// the radio must never be blocked waiting on a slow listener.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Millisecond):
			}
			continue
		}

		n, overflow, err := s.readBlock(block)
		switch {
		case err != nil:
			block.Release()
			if consecutive++; consecutive >= maxConsecutiveErrors {
				return err
			}
			continue
		case n == 0:
			block.Release()
			continue
		}
		consecutive = 0

		block.Samples = block.full[:n]
		block.Overflow = overflow

		select {
		case out <- block:
		case <-ctx.Done():
			block.Release()
			return nil
		}
	}
	return nil
}

// readBlock performs one readStream call and copies the result into block.
func (s *SoapySource) readBlock(block *Block) (n int, overflow bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dev == nil || s.stream == nil {
		return 0, false, errors.New("device is not open")
	}

	var flags C.int
	var timeNs C.longlong
	ret := C.SoapySDRDevice_readStream(s.dev, s.stream, s.buffs,
		C.size_t(s.blockSize), &flags, &timeNs, C.long(readTimeoutUS))

	switch {
	case ret > 0:
		// Never trust the driver's count: it indexes a buffer sized for the
		// request. See ClampSampleCount.
		got := ClampSampleCount(int(ret), s.blockSize)
		copy(block.full[:got], unsafe.Slice((*complex64)(s.cbuf), s.blockSize)[:got])
		return got, false, nil

	case ret == C.SOAPY_SDR_TIMEOUT:
		// No samples this time; entirely normal while the device warms up.
		return 0, false, nil

	case ret == C.SOAPY_SDR_OVERFLOW:
		// The host fell behind and the device discarded samples. Keep going,
		// but count it: silent sample loss is otherwise indistinguishable from
		// a poor antenna.
		s.overflows.Add(1)
		return 0, true, nil

	default:
		return 0, false, fmt.Errorf("readStream: %s (%s)",
			C.GoString(C.SoapySDR_errToStr(ret)), lastError())
	}
}

// reconnect closes everything and opens the device again from scratch, which is
// what a USB re-enumeration requires.
func (s *SoapySource) reconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closeDevice()
	if err := s.open(); err != nil {
		return err
	}
	s.allocBuffers()
	return nil
}
