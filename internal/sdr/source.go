// Package sdr provides IQ sample sources.
//
// The Source interface exists so the rest of the program runs identically
// against real hardware and against recorded IQ. That is not a testing
// nicety: it is what lets the DSP, the streaming layer and the browser client
// all be developed and regression-tested with no radio attached, against a
// signal that is byte-for-byte reproducible.
package sdr

import (
	"context"
	"encoding/binary"
	"math"
	"sync/atomic"

	"github.com/bitmorse/airport-sdr/internal/assert"
)

// SampleBytes is the size of one complex sample in cf32 form: two little-endian
// float32s, real first. This is what rx_sdr and GNU Radio read and write.
const SampleBytes = 8

// Source produces baseband IQ at a fixed sample rate and centre frequency.
//
// Blocks received from Start must be returned with Release once consumed. The
// pool behind them is fixed-size, so a consumer that forgets will starve the
// producer rather than grow the heap.
type Source interface {
	// Start begins streaming. The returned channel is closed when the source
	// stops, whether through context cancellation, end of input, or error.
	Start(ctx context.Context) (<-chan *Block, error)
	SampleRate() float64
	CenterFreq() float64

	// Retune moves the source to a new centre frequency and sample rate.
	//
	// It must NOT be called while a stream is running: cancel the context
	// passed to Start and wait for its channel to close first. Retuning a live
	// device would pull the hardware out from under the receive loop, and the
	// samples either side of the change belong to different bands anyway.
	//
	// On failure the source is left on its previous tuning where possible, so a
	// rejected retune does not take a working receiver down with it.
	Retune(centerFreq, sampleRate float64) error

	// Describe returns a one-line summary for logs and /api/status.
	Describe() string
	Close() error
}

// Block is one batch of complex baseband samples, owned by a BlockPool.
type Block struct {
	Samples []complex64
	// Overflow reports that the driver dropped samples before this block. The
	// chain keeps running; it is surfaced for diagnostics because silent
	// sample loss is otherwise indistinguishable from a bad antenna.
	Overflow bool

	pool *BlockPool
	full []complex64 // the block's true extent, restored on release
	held atomic.Bool
}

// Release returns the block to its pool. It is safe to call more than once;
// a duplicate release is ignored rather than corrupting the free list.
func (b *Block) Release() {
	if b.pool == nil {
		return
	}
	if !b.held.CompareAndSwap(true, false) {
		return // already released
	}
	b.Overflow = false
	b.Samples = b.full

	select {
	case b.pool.free <- b:
	default:
		// Unreachable: the channel has room for every block the pool created.
		assert.That(false, "block pool free list overflowed")
	}
}

// BlockPool hands out a fixed set of preallocated blocks.
//
// Fixed size is the point. Power of 10 rule 3 asks for no allocation after
// initialisation, and on a 512 MB device it is also what stops a stalled
// consumer from growing the heap without bound. Exhaustion is reported to the
// caller so it can drop samples deliberately instead of blocking the radio.
type BlockPool struct {
	free chan *Block
	size int
}

// NewBlockPool preallocates count blocks of blockSize samples each.
func NewBlockPool(count, blockSize int) *BlockPool {
	assert.Thatf(count > 0, "block pool needs at least one block, got %d", count)
	assert.Thatf(blockSize > 0, "block size must be positive, got %d", blockSize)

	p := &BlockPool{free: make(chan *Block, count), size: blockSize}
	for i := 0; i < count; i++ {
		b := &Block{pool: p, full: make([]complex64, blockSize)}
		b.Samples = b.full
		p.free <- b
	}
	return p
}

// Get returns a free block, or false when every block is still in use.
func (p *BlockPool) Get() (*Block, bool) {
	select {
	case b := <-p.free:
		b.held.Store(true)
		return b, true
	default:
		return nil, false
	}
}

// Free reports how many blocks are currently available.
func (p *BlockPool) Free() int { return len(p.free) }

// BlockSize reports the sample count of blocks from this pool.
func (p *BlockPool) BlockSize() int { return p.size }

// BlockSizeFor returns a sensible block length for a sample rate: roughly
// 20 ms of samples, which matches the order of magnitude a driver hands back
// and keeps latency low, with a floor for very low rates.
func BlockSizeFor(sampleRate float64) int {
	n := int(sampleRate * defaultBlockDuration.Seconds())
	if n < minBlockSize {
		n = minBlockSize
	}
	return n
}

// EncodeCF32 writes src into dst as interleaved little-endian float32 pairs and
// returns the number of samples written, which is limited by whichever of the
// two buffers runs out first. It allocates nothing.
func EncodeCF32(dst []byte, src []complex64) int {
	n := len(src)
	if room := len(dst) / SampleBytes; n > room {
		n = room
	}
	for i := 0; i < n; i++ {
		s := src[i]
		binary.LittleEndian.PutUint32(dst[i*SampleBytes:], math.Float32bits(real(s)))
		binary.LittleEndian.PutUint32(dst[i*SampleBytes+4:], math.Float32bits(imag(s)))
	}
	assert.That(n <= len(src), "encoded more samples than were supplied")
	return n
}

// DecodeCF32 reads interleaved little-endian float32 pairs from src into dst and
// returns the number of samples decoded. A trailing partial sample is ignored.
// It allocates nothing.
func DecodeCF32(dst []complex64, src []byte) int {
	n := len(src) / SampleBytes
	if len(dst) < n {
		n = len(dst)
	}
	for i := 0; i < n; i++ {
		re := math.Float32frombits(binary.LittleEndian.Uint32(src[i*SampleBytes:]))
		im := math.Float32frombits(binary.LittleEndian.Uint32(src[i*SampleBytes+4:]))
		dst[i] = complex(re, im)
	}
	assert.That(n*SampleBytes <= len(src), "decoded past the end of the input")
	return n
}
