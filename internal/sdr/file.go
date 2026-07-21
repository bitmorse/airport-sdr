package sdr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/bitmorse/airport-sdr/internal/assert"
)

const (
	// defaultBlockDuration keeps blocks at roughly 20 ms, matching the order of
	// magnitude a hardware driver hands back and keeping latency low.
	defaultBlockDuration = 20 * time.Millisecond
	defaultPoolBlocks    = 8
	minBlockSize         = 256

	// maxFillIterations bounds the read loop (Power of 10 rule 2). A file
	// shorter than one block is legitimate and needs several wraps to fill it,
	// but the count can never be unbounded.
	maxFillIterations = 1024
)

// FileOptions configures a FileSource.
type FileOptions struct {
	Path       string
	SampleRate float64
	CenterFreq float64
	// Loop restarts at the beginning on EOF instead of ending the stream.
	Loop bool
	// BlockSize in samples; zero derives it from the sample rate.
	BlockSize int
	// PoolSize is the number of preallocated blocks; zero uses the default.
	PoolSize int
	// Realtime paces replay to the sample rate. Tests leave it off to run at
	// full speed; anything standing in for a live radio should set it.
	Realtime bool
}

// FileSource replays interleaved-float32 IQ from disk, optionally looping.
//
// This is the development source, and the most useful thing in the package: a
// recorded capture is a reproducible signal, so DSP changes can be compared
// against a known-good result rather than against whatever happened to be on
// the air at the time.
type FileSource struct {
	opts FileOptions
	f    *os.File
	pool *BlockPool
	raw  []byte // reused read buffer; the hot path allocates nothing

	closeOnce sync.Once
}

// NewFileSource opens path and validates the replay parameters.
func NewFileSource(opts FileOptions) (*FileSource, error) {
	if opts.SampleRate <= 0 {
		return nil, fmt.Errorf("sample rate must be positive, got %v", opts.SampleRate)
	}
	if opts.BlockSize <= 0 {
		opts.BlockSize = BlockSizeFor(opts.SampleRate)
	}
	if opts.PoolSize <= 0 {
		opts.PoolSize = defaultPoolBlocks
	}

	f, err := os.Open(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("open iq file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close() // already failing; the close error adds nothing
		return nil, fmt.Errorf("stat iq file: %w", err)
	}
	if info.Size() < SampleBytes {
		_ = f.Close()
		return nil, fmt.Errorf("iq file %s holds no complete samples", opts.Path)
	}

	return &FileSource{
		opts: opts,
		f:    f,
		pool: NewBlockPool(opts.PoolSize, opts.BlockSize),
		raw:  make([]byte, opts.BlockSize*SampleBytes),
	}, nil
}

func (s *FileSource) SampleRate() float64 { return s.opts.SampleRate }
func (s *FileSource) CenterFreq() float64 { return s.opts.CenterFreq }

// Retune accepts only the tuning the capture was recorded with.
//
// A file holds one fixed slice of spectrum. Silently accepting another tuning
// would demodulate the wrong offsets from the right samples and produce
// confident nonsense, so switching to a group this capture cannot serve is an
// error the caller has to handle.
func (s *FileSource) Retune(centerFreq, sampleRate float64) error {
	if math.Abs(centerFreq-s.opts.CenterFreq) > hzTolerance ||
		math.Abs(sampleRate-s.opts.SampleRate) > hzTolerance {
		return fmt.Errorf(
			"capture %s holds %.4f MHz at %.3f MS/s and cannot be retuned to "+
				"%.4f MHz at %.3f MS/s; record that group separately to replay it",
			s.opts.Path, s.opts.CenterFreq/1e6, s.opts.SampleRate/1e6,
			centerFreq/1e6, sampleRate/1e6)
	}
	return nil
}

func (s *FileSource) Describe() string {
	mode := "once"
	if s.opts.Loop {
		mode = "looping"
	}
	return fmt.Sprintf("file %s (%s) @ %.3f MS/s, centre %.4f MHz",
		s.opts.Path, mode, s.opts.SampleRate/1e6, s.opts.CenterFreq/1e6)
}

func (s *FileSource) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.f.Close() })
	return err
}

// Start replays the file until it ends, the context is cancelled, or an error
// occurs. The returned channel is closed on the way out in every case.
func (s *FileSource) Start(ctx context.Context) (<-chan *Block, error) {
	out := make(chan *Block)
	go s.run(ctx, out)
	return out, nil
}

func (s *FileSource) run(ctx context.Context, out chan<- *Block) {
	defer close(out)

	var pace *time.Ticker
	if s.opts.Realtime {
		period := time.Duration(float64(s.opts.BlockSize) / s.opts.SampleRate * float64(time.Second))
		pace = time.NewTicker(period)
		defer pace.Stop()
	}

	for {
		if pace != nil {
			select {
			case <-pace.C:
			case <-ctx.Done():
				return
			}
		}

		block, ok := s.acquire(ctx)
		if !ok {
			return
		}

		n, err := s.fill(block.full)
		if n == 0 {
			block.Release()
			return
		}
		block.Samples = block.full[:n]

		select {
		case out <- block:
		case <-ctx.Done():
			block.Release()
			return
		}
		if err != nil {
			return // short final block already delivered
		}
	}
}

// acquire waits for a free block, giving a stalled consumer a chance to catch
// up. It gives up when the context ends.
func (s *FileSource) acquire(ctx context.Context) (*Block, bool) {
	for {
		if b, ok := s.pool.Get(); ok {
			return b, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(time.Millisecond):
		}
	}
}

// fill reads one block's worth of samples into dst, wrapping to the start of
// the file when looping. It returns the sample count, and a non-nil error only
// when the stream should end after this block.
func (s *FileSource) fill(dst []complex64) (int, error) {
	got := 0
	for iter := 0; got < len(s.raw) && iter < maxFillIterations; iter++ {
		n, err := s.f.Read(s.raw[got:])
		got += n

		if errors.Is(err, io.EOF) {
			if !s.opts.Loop {
				return DecodeCF32(dst, s.raw[:got]), io.EOF
			}
			if _, serr := s.f.Seek(0, io.SeekStart); serr != nil {
				return DecodeCF32(dst, s.raw[:got]), serr
			}
			continue
		}
		if err != nil {
			return DecodeCF32(dst, s.raw[:got]), err
		}
	}
	assert.That(got <= len(s.raw), "read past the end of the scratch buffer")
	return DecodeCF32(dst, s.raw[:got]), nil
}
