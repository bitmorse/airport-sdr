# Hardware

## Tested

### LimeSDR Mini v2.0 — ✅ verified working

Confirmed receiving live tower audio at 118.100 MHz.

```yaml
sdr:
  driver: "driver=lime"
  sample_rate: 960000
  center_freq: 118250000
  gain: 61              # the device maximum
  antenna: "LNAW"       # the wideband port; the default is not this
```

Needs `libsoapysdr-dev` and `soapysdr-module-lms7`. `probe` reports:

```
antennas      NONE, LNAH, LNAL, LNAW, LB1, LB2
gain range    -12-61
sample rates  100000-61440000
frequencies   0-3800000000
```

### Not yet tested

RTL-SDR, Airspy, HackRF, bladeRF are all supported by SoapySDR and should work.
RTL-SDR reports sample rates as a **fixed list** rather than a range, so choose
one that `probe` prints — a rate the device does not support is refused outright
rather than silently adjusted, because the DSP chain depends on the exact value.

## The antenna trap

**This is the failure to check first.** On a radio with several antenna ports,
leaving `sdr.antenna` empty lets the driver choose, and its choice is often wrong
for VHF. A LimeSDR Mini even offers a port literally called `NONE`.

Measured on this hardware, same signal, same minute:

| Configuration | Peak wideband level |
|---|---|
| driver default port, gain 40 | **−85.6 dBFS** (noise floor) |
| `LNAW`, gain 61 | **−21.4 dBFS** |

That is a 64 dB difference, and the symptom is simply "no audio" — which looks
identical to a quiet frequency. Always set the port explicitly, and run `probe`
on any new radio to see what it offers.

## Diagnosing a silent channel

There are three distinct causes and they need different fixes. `replay` prints
the two numbers that separate them:

```
peak wideband level -21.4 dBFS  (whole captured span)
peak channel level  -59.2 dBFS  (after the channel filter)
squelch open for 0.0% of the capture
```

| Symptom | Cause | Fix |
|---|---|---|
| Wideband at the noise floor (< −60) | Receiver is deaf | Antenna port, gain, or a disconnected cable |
| Wideband strong, channel ≫20 dB below | Frequency was quiet | Record for longer |
| Both healthy, squelch still shut | Threshold too high | Lower `squelch_db` below the channel peak |

`spectrum` settles it visually. Real output from a working setup:

```
 118.0850 MHz   -72.2   -62.5 |#############-------            <= Tower
 118.2350 MHz   -65.6   -33.6 |##################------------  <= LO (DC spur)
 118.2650 MHz   -59.7   -27.6 |######################--------  <= LO (DC spur)
```

`#` is the average level, `-` extends it to the max-hold. Max-hold matters:
a five-second transmission inside a minute of silence nearly vanishes from an
average but stands out clearly as a peak.

## The DC spur

Note the two rows above at the local oscillator: **−27.6 dBFS peak against a
−87 dBFS noise floor, roughly 60 dB of spur.** Almost every direct-conversion
front end has this, and any channel sitting on it is unreceivable.

The fix is offset tuning: put the tuner *between* the channels you want. Centring
on 118.250 places Tower (118.100) and Ground (118.400) both 150 kHz clear of it,
in the same capture. Config validation enforces a 12.5 kHz minimum offset and
refuses to start otherwise.

## Signal strength expectations

At the test site, with the antenna indoors:

| | |
|---|---|
| Channel noise floor | ≈ −67 dBFS |
| Tower transmission peaks | ≈ −59 dBFS |
| Usable SNR | **≈ 11 dB** |

That 8 dB window between floor and signal is what `squelch_db` has to land in,
and it is why the threshold cannot be a fixed default.

**No software setting improves this.** RF gain is already at the device maximum.
More margin means a better antenna: a proper airband quarter-wave or a
purpose-built VHF airband antenna, ideally outdoors and clear of buildings. An
external LNA helps only if feedline loss is the limiting factor.

## Edge devices

The intended target is Pi Zero 2 W class hardware. Two constraints matter there:

- **USB bandwidth.** Keep the sample rate low — 960 kS/s covers a 300 kHz span
  of airband and is gentle on a shared USB 2.0 bus. 8 MS/s would swamp it.
- **No active cooling.** The DSP measures 167 µs per 20 ms block on a
  workstation (0.84% of one core). A Cortex-A53 is perhaps 10–15× slower per
  core, so expect roughly 10–15% per channel — **an estimate that has not yet
  been measured on real hardware.**
