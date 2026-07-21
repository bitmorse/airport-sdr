# The signal chain

Everything here lives in `internal/dsp/`. Each stage is a plain struct with a
`Process` method that appends to a caller-supplied slice; nothing allocates once
constructed.

## Overview

At the default 960 kS/s, one channel:

```
960 kS/s CF32 → NCO shift → /20 → 48 kHz → /2 → 24 kHz → |z| → DC block → squelch
               (−150 kHz)                                  (AM)  (~30 Hz HP)  (+hang)
                                                                     ↓
                        8 kHz audio ← /3 ← LPF 3.4 kHz ← limiter ← AGC
```

The decimation factors are not hardcoded. `PlanDecimation` derives them from the
configured rates, so 2.4 MS/s resolves to `/25, /4, demod, /3` through the same
code. Running both configurations through one planner is what validates it.

## Stage by stage

### NCO — frequency shift

The tuner sits deliberately away from every channel to avoid the DC spur, so each
channel is shifted down to zero before filtering. `NewNCO(-offset, fs)`: to bring
a channel at +150 kHz down to DC, shift by −150 kHz.

The phasor advances by repeated complex multiplication and is renormalised every
1024 samples. Without that it drifts off the unit circle — imperceptibly over a
thousand samples, unacceptably over the billions a device sees between restarts.

### Decimating FIR — narrowing to the channel

Only the samples that survive decimation are ever computed. Filtering at the
input rate and discarding most results costs the decimation factor in wasted
multiplies, which on a Pi is the difference between viable and not.

**Filter design is the non-obvious part.** Each stage only needs to reject what
would *fold back onto the channel*, not everything above its output Nyquist. So
the stopband starts at `outputRate − protectedBand` rather than at
`outputRate / 2`. That widens the transition band enormously:

| | Naive (cutoff at 0.4×rate) | Actual (protect ±5.1 kHz) |
|---|---|---|
| Stage 1 taps | ~550 | **~140** |
| Cost | ~26 MMAC/s | **~6.7 MMAC/s** |

Roughly 4× cheaper for identical audio.

Filters keep a history of `taps−1` samples across calls, so **output does not
depend on how input was chunked** — a property with its own test, because a
block-boundary bug produces audio that sounds almost right, which is the worst
kind of wrong.

### AM demodulation

Envelope detection: the magnitude of each complex sample. That is the whole of
it, and the reason AM airband is so easy to receive. The carrier survives as a DC
offset, removed next.

### DC block

A one-pole high-pass at ~30 Hz, `(1 − z⁻¹)/(1 − r·z⁻¹)`: a zero at DC to kill the
carrier, a pole just inside it so the voice band passes untouched. Low enough to
leave speech alone, high enough to track a carrier shifting as a signal fades.

### Squelch

**Not optional.** An airband channel is silent well over 90% of the time, and an
unsquelched AGC chases the noise floor to full gain, producing a hiss that makes
the receiver unusable.

Three behaviours, each with tests:

- **Hysteresis** (3 dB) — a signal hovering at the threshold would otherwise
  chatter open and shut, which is worse to listen to than plain noise.
- **Hang time** (0.3 s) — speech has natural gaps; closing instantly on every
  pause chops words apart.
- **Sample-counted timing**, not wall-clock, so behaviour is deterministic and
  identical whether replaying a capture at speed or receiving live.

### AGC

Peak-following envelope detector with immediate attack and a 0.2 s release, plus
a hard limiter. Airband needs it badly: a tower a few miles away and an aircraft
at altitude can differ by 40 dB.

`MaxGain` is **4000**. That is not arbitrary — a real capture measured a channel
peak of −59 dBFS, which needs roughly 900× to reach the AGC target. An earlier
cap of 200 left genuine traffic about 13 dB too quiet.

**The AGC holds on carrier presence, not on squelch state.** This distinction
matters: the squelch stays open through its hang window, and letting the AGC
adapt during that time would wind gain up on noise, so the first syllable of the
next transmission arrives massively overdriven. An AM transmitter keys its
carrier for the whole transmission including pauses between words, so carrier
presence tracks gain correctly through speech gaps and freezes it the moment the
transmission ends.

## Decimation planner

`PlanDecimation(inputRate, audioRate)` splits the total ratio into stages:

1. The ratio must be a **whole number** — refused otherwise, with a message.
2. Pick the post-demodulation factor (prefers 3, giving a 24 kHz IF at 8 kHz
   audio — comfortably wide for the channel and cheap to filter).
3. Factor the rest into stages of at most 32, largest first, so the sample rate
   falls as early as possible and later filters are cheap.

| Input | Stages | IF |
|---|---|---|
| 960 kS/s | /20, /2, demod, /3 | 24 kHz |
| 1.024 MS/s | /32, demod, /4 | 32 kHz |
| 2.048 MS/s | /32, /2, demod, /4 | 32 kHz |
| 2.4 MS/s | /25, /4, demod, /3 | 24 kHz |

## Performance

Measured on a workstation, 20 ms blocks at 960 kS/s, one channel:

```
BenchmarkChannelProcess    167393 ns/op    0 B/op    0 allocs/op
BenchmarkFIRDecimC         147589 ns/op    0 B/op    0 allocs/op
BenchmarkNCOMix             61200 ns/op    0 B/op    0 allocs/op
```

167 µs per 20 ms block is 0.84% of one core. A Cortex-A53 is perhaps 10–15×
slower per core, so budget 10–15% per channel on a Pi — **not yet measured on
real hardware**.

Zero allocations is a *test*, not a benchmark note: on a 512 MB device,
allocation churn in the audio path means the garbage collector runs constantly.

## Diagnostics

`internal/dsp/fft.go` is diagnostic machinery, not part of the receive chain. It
runs only for the `spectrum` command, so it is written for clarity rather than
under the allocation discipline the rest of the package follows.

It provides an averaged spectrum and a **max-hold**. Max-hold is what makes
intermittent traffic visible: a five-second transmission inside a minute-long
capture is averaged almost to nothing, but stands out clearly as a peak.
