package dsp

import (
	"fmt"
	"math"

	"github.com/bitmorse/airport-sdr/internal/assert"
)

const (
	// MaxStageDecim bounds a single decimation stage. Splitting a large ratio
	// across stages keeps every filter short: a stage's cost is its output rate
	// times its tap count, so decimating early and filtering narrowly later is
	// dramatically cheaper than doing it all at once.
	MaxStageDecim = 32

	// MinIFRate is the lowest rate at which AM demodulation still has room for
	// the channel either side of the carrier.
	MinIFRate = 16_000.0
	MaxIFRate = 64_000.0

	MaxIFStages = 3

	// DefaultAudioBandwidth is the top of the voice band. Airband AM carries
	// nothing useful above this.
	DefaultAudioBandwidth = 3_400.0

	// ifProtectRatio widens the band each IF stage must preserve beyond the
	// audio bandwidth, leaving room for both AM sidebands plus margin.
	ifProtectRatio = 1.5

	defaultHangTime     = 0.3 // seconds
	defaultHysteresisDB = 3.0
	defaultAGCTarget    = 0.5
	// A distant aircraft can land only a few dB above the noise floor, and a
	// real capture measured a channel peak of -59 dBFS. Reaching the AGC target
	// from there needs roughly 900x, so a cap of a few hundred would leave weak
	// traffic audibly quiet. The squelch is what stops this much gain being
	// applied to noise.
	defaultAGCMaxGain = 4000.0
	defaultAGCRelease = 0.2 // seconds
)

// Plan describes how a chain gets from the radio's sample rate down to audio.
type Plan struct {
	InputRate float64
	AudioRate float64
	// IFDecims are the complex decimation stages applied before demodulation.
	IFDecims []int
	// IFRate is the sample rate at which demodulation happens.
	IFRate float64
	// AudioDecim is the single real decimation applied after demodulation.
	AudioDecim int
}

// PlanDecimation works out the decimation stages between inputRate and
// audioRate.
//
// The chain decimates by whole numbers throughout, so the ratio must be exact.
// Rejecting a bad ratio here, with the numbers in hand, gives a far better
// error than discovering it deep in a filter constructor.
func PlanDecimation(inputRate, audioRate float64) (Plan, error) {
	if inputRate <= 0 || audioRate <= 0 {
		return Plan{}, fmt.Errorf("rates must be positive, got input %v and audio %v",
			inputRate, audioRate)
	}
	if inputRate < audioRate {
		return Plan{}, fmt.Errorf("input rate %v is below the audio rate %v", inputRate, audioRate)
	}

	ratio := inputRate / audioRate
	total := int(math.Round(ratio))
	if math.Abs(ratio-float64(total)) > 1e-9 {
		return Plan{}, fmt.Errorf(
			"input rate %v is not a whole multiple of the audio rate %v (ratio %.4f); "+
				"pick a sample rate divisible by %v", inputRate, audioRate, ratio, audioRate)
	}

	// Prefer an IF of three times the audio rate: at 8 kHz audio that is
	// 24 kHz, comfortably wide for the channel and cheap to filter.
	for _, audioDecim := range []int{3, 4, 2, 5} {
		if total%audioDecim != 0 {
			continue
		}
		ifRate := audioRate * float64(audioDecim)
		if ifRate < MinIFRate || ifRate > MaxIFRate {
			continue
		}
		stages, ok := factorStages(total / audioDecim)
		if !ok {
			continue
		}
		return Plan{
			InputRate: inputRate, AudioRate: audioRate,
			IFDecims: stages, IFRate: ifRate, AudioDecim: audioDecim,
		}, nil
	}

	return Plan{}, fmt.Errorf(
		"cannot decimate %v to %v in whole stages; try 960000, 1024000, 2048000 or 2400000",
		inputRate, audioRate)
}

// factorStages splits n into at most MaxIFStages factors, each no larger than
// MaxStageDecim, taking the largest usable factor first so the sample rate
// falls as early as possible.
func factorStages(n int) ([]int, bool) {
	var stages []int
	for n > 1 {
		if len(stages) >= MaxIFStages {
			return nil, false
		}
		factor := 0
		for d := min(MaxStageDecim, n); d >= 2; d-- {
			if n%d == 0 {
				factor = d
				break
			}
		}
		if factor == 0 {
			return nil, false // n is prime and larger than MaxStageDecim
		}
		stages = append(stages, factor)
		n /= factor
	}
	return stages, true
}

// ChannelOptions configures one demodulated channel.
type ChannelOptions struct {
	Name string
	// Offset is the channel's distance from the tuner's centre frequency, in
	// hertz. Negative means below centre.
	Offset          float64
	InputRate       float64
	AudioRate       float64
	SquelchDB       float64
	HysteresisDB    float64
	HangTime        float64 // seconds; zero uses the default
	AudioBandwidth  float64 // zero uses DefaultAudioBandwidth
	MaxInputSamples int
}

// Channel is the full receive chain for one frequency: shift, decimate,
// demodulate, squelch, gain-control and band-limit.
//
// One Channel handles one frequency, and several can share a single capture as
// long as they all fall inside it. Every buffer is sized at construction, so
// Process allocates nothing.
type Channel struct {
	name   string
	plan   Plan
	maxIn  int
	sqchDB float64

	nco      *NCO
	stages   []*FIRDecimC
	stageBuf [][]complex64
	mixBuf   []complex64

	demod    []float32
	dc       *DCBlock
	audioFIR *FIRDecimR
	audioBuf []float32
	agc      *AGC
	squelch  *Squelch

	out   []float32
	level float64
}

// NewChannel builds a chain for one channel.
func NewChannel(opts ChannelOptions) (*Channel, error) {
	if opts.MaxInputSamples <= 0 {
		return nil, fmt.Errorf("channel %q: MaxInputSamples must be positive", opts.Name)
	}
	if math.Abs(opts.Offset) > opts.InputRate/2 {
		return nil, fmt.Errorf(
			"channel %q: offset %.1f kHz is outside the captured band of +/-%.1f kHz",
			opts.Name, opts.Offset/1e3, opts.InputRate/2e3)
	}
	plan, err := PlanDecimation(opts.InputRate, opts.AudioRate)
	if err != nil {
		return nil, fmt.Errorf("channel %q: %w", opts.Name, err)
	}
	opts = applyChannelDefaults(opts)

	c := &Channel{
		name:   opts.Name,
		plan:   plan,
		maxIn:  opts.MaxInputSamples,
		sqchDB: opts.SquelchDB,
		// The tuner sits away from the channel to keep it off the DC spur, so
		// shifting by the negated offset brings the channel down to zero.
		nco:    NewNCO(-opts.Offset, opts.InputRate),
		mixBuf: make([]complex64, 0, opts.MaxInputSamples),
	}
	c.buildIFStages(opts)
	c.buildAudioStages(opts)
	return c, nil
}

func applyChannelDefaults(o ChannelOptions) ChannelOptions {
	if o.AudioBandwidth <= 0 {
		o.AudioBandwidth = DefaultAudioBandwidth
	}
	if o.HangTime <= 0 {
		o.HangTime = defaultHangTime
	}
	if o.HysteresisDB <= 0 {
		o.HysteresisDB = defaultHysteresisDB
	}
	return o
}

// buildIFStages creates the complex decimation chain and its buffers.
func (c *Channel) buildIFStages(opts ChannelOptions) {
	protect := opts.AudioBandwidth * ifProtectRatio

	rate := opts.InputRate
	maxSamples := opts.MaxInputSamples

	for _, decim := range c.plan.IFDecims {
		out := rate / float64(decim)

		// Only content that would fold back onto the channel needs rejecting,
		// so the stopband starts at out-protect rather than at out/2. That
		// widens the transition band enormously and buys back most of the taps.
		transition := out - 2*protect
		assert.Thatf(transition > 0, "IF stage output %v too low for protected band %v", out, protect)

		taps := DesignLowPass(rate, protect, TapsForTransition(rate, transition))
		stage := NewFIRDecimC(taps, decim)

		maxSamples = stage.MaxOutputLen(maxSamples)
		c.stages = append(c.stages, stage)
		c.stageBuf = append(c.stageBuf, make([]complex64, 0, maxSamples))
		rate = out
	}

	c.demod = make([]float32, 0, maxSamples)
}

// buildAudioStages creates everything after demodulation.
func (c *Channel) buildAudioStages(opts ChannelOptions) {
	ifSamples := cap(c.demod)

	// Anything above audioRate-bandwidth folds into the voice band once
	// decimated, so that is where the stopband must begin.
	transition := opts.AudioRate - 2*opts.AudioBandwidth
	assert.Thatf(transition > 0, "audio rate %v too low for bandwidth %v",
		opts.AudioRate, opts.AudioBandwidth)

	taps := DesignLowPass(c.plan.IFRate, opts.AudioBandwidth,
		TapsForTransition(c.plan.IFRate, transition))

	c.dc = NewDCBlock(30, c.plan.IFRate)
	c.audioFIR = NewFIRDecimR(taps, c.plan.AudioDecim)
	c.audioBuf = make([]float32, 0, c.audioFIR.MaxOutputLen(ifSamples))
	c.out = make([]float32, 0, cap(c.audioBuf))

	c.agc = NewAGC(AGCConfig{
		SampleRate:  opts.AudioRate,
		Target:      defaultAGCTarget,
		MaxGain:     defaultAGCMaxGain,
		ReleaseTime: defaultAGCRelease,
	})
	c.squelch = NewSquelch(SquelchConfig{
		ThresholdDB:  opts.SquelchDB,
		HysteresisDB: opts.HysteresisDB,
		HangSamples:  int(opts.HangTime * c.plan.IFRate),
	})
}

func (c *Channel) Name() string     { return c.name }
func (c *Channel) Plan() Plan       { return c.plan }
func (c *Channel) Open() bool       { return c.squelch.Open() }
func (c *Channel) LevelDB() float64 { return c.level }
func (c *Channel) Gain() float32    { return c.agc.Gain() }

// Process runs one block of baseband IQ through the chain and returns the
// resulting audio.
//
// The returned slice is owned by the Channel and is valid only until the next
// call; copy it if it must outlive that. Input longer than MaxInputSamples is
// handled in pieces rather than rejected, so an oversized block degrades to a
// little extra copying instead of a panic.
func (c *Channel) Process(iq []complex64) []float32 {
	c.out = c.out[:0]
	for off := 0; off < len(iq); {
		n := min(len(iq)-off, c.maxIn)
		c.out = append(c.out, c.processChunk(iq[off:off+n])...)
		off += n
	}
	return c.out
}

func (c *Channel) processChunk(iq []complex64) []float32 {
	cur := c.nco.Mix(c.mixBuf[:0], iq)
	for i, stage := range c.stages {
		cur = stage.Process(c.stageBuf[i][:0], cur)
	}

	c.level = LevelDB(cur)

	// Hold the AGC on carrier presence rather than on the squelch, which stays
	// open through its hang window. An AM transmitter keys the carrier for the
	// whole transmission, including pauses between words, so this tracks gain
	// through natural speech gaps while still freezing it the moment the
	// transmission ends and only noise remains.
	c.agc.SetHold(c.level < c.sqchDB)
	open := c.squelch.Update(c.level, len(cur))

	audio := Demodulate(c.demod[:0], cur)
	c.dc.Process(audio)
	audio = c.audioFIR.Process(c.audioBuf[:0], audio)
	c.agc.Process(audio)

	if !open {
		for i := range audio {
			audio[i] = 0
		}
	}
	return audio
}
