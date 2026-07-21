# Configuration

One YAML file. Validated on load; **unknown keys are an error**, because a
silently ignored typo is worse than one that fails loudly.

```bash
airport-sdr --config configs/config.yaml validate
```

Validation reports every problem at once, not just the first, so fixing a config
is one edit rather than several rounds.

## Full reference

### `sdr`

| Key | Type | Default | Notes |
|---|---|---|---|
| `driver` | string | `""` | SoapySDR device args, e.g. `driver=lime`. Naming the driver also stops SoapySDR probing every installed module, which is the source of the ALSA/SSDP error wall at startup. |
| `sample_rate` | Hz | `960000` | **Must be a whole multiple of `audio.rate`.** |
| `center_freq` | Hz | `118250000` | Must not be on a channel — see below. |
| `gain` | dB | `61` | Range −50…100 here; clamped to what the device reports. Negative is legal — some front ends express attenuation that way. |
| `auto_gain` | bool | `false` | Hands gain to the driver; `gain` is then ignored. |
| `antenna` | string | `"LNAW"` | **Set this explicitly.** See [hardware.md](hardware.md). |
| `ppm` | float | `0` | Crystal correction. Not every driver supports it; a refusal is a warning, not fatal. |

### `audio`

| Key | Type | Default | Notes |
|---|---|---|---|
| `rate` | Hz | `8000` | Ample for airband voice, which is bandlimited to ~3.4 kHz. |

### `server`

| Key | Type | Default | Notes |
|---|---|---|---|
| `listen` | addr | `127.0.0.1:8080` | Loopback by default. Exposure must be deliberate. |

### `channels[]`

| Key | Type | Default | Notes |
|---|---|---|---|
| `name` | string | — | Required, unique. Used in URLs. |
| `freq` | Hz | — | Required. Must be inside the captured span and clear of the LO. |
| `mode` | string | `am` | Only `am` today. |
| `squelch_db` | dBFS | `-35` | **Measure this per site.** See below. |

## Rules the validator enforces

### The tuner must not sit on a channel

Direct-conversion front ends have a DC spur at the local oscillator — measured
here at 60 dB above the noise floor. A channel parked there is unreceivable, and
nothing downstream can recover it.

Every channel must be at least **12.5 kHz** from `center_freq`. Offset-tune:

```yaml
sdr:
  center_freq: 118250000   # between the two channels, on neither
channels:
  - {name: Tower,  freq: 118100000, mode: am, squelch_db: -62}   # -150 kHz
  - {name: Ground, freq: 118400000, mode: am, squelch_db: -62}   # +150 kHz
```

### Channels must fit the captured span

A channel must be within ±40% of the sample rate from centre — the outer edges
are where the anti-aliasing filter rolls off. At 960 kS/s that is ±384 kHz.

### Sample rate must divide evenly into audio rate

The chain decimates by integers throughout. `1234567 / 8000` is not a whole
number, so it is refused with a message rather than rounded — a rate that was
quietly adjusted would produce audio at subtly the wrong speed, which is
miserable to diagnose by ear.

Rates known to work: `960000`, `1024000`, `2048000`, `2400000`.

## Choosing `squelch_db`

**There is no good default.** It depends on the antenna, the gain, the site and
the local noise environment. Getting it wrong fails silently in both directions:
too high and you hear nothing, too low and the channel hisses continuously.

Measure it. Record a capture, then:

```bash
make record DURATION=120s
make replay
```

```
peak wideband level -21.4 dBFS
peak channel level  -59.2 dBFS
squelch open for 0.0% of the capture
```

Set the threshold a few dB below the **channel** peak. At the test site the
sweep looked like this:

| `squelch_db` | Squelch open |
|---|---|
| −50 | 0% — too high, traffic missed |
| −60 | 17.9% |
| **−62** | **19.5% — chosen** |
| −65 | 23.9% |
| −68 | 99.2% — below the noise floor, permanently open |

The floor is wherever the figure jumps toward 100%. Sit a few dB above it.

## Examples

**LimeSDR Mini, two airband channels:**

```yaml
sdr:
  driver: "driver=lime"
  sample_rate: 960000
  center_freq: 118250000
  gain: 61
  antenna: "LNAW"
audio:
  rate: 8000
server:
  listen: "127.0.0.1:8080"
channels:
  - {name: Tower,  freq: 118100000, mode: am, squelch_db: -62}
  - {name: Ground, freq: 118400000, mode: am, squelch_db: -62}
```

**RTL-SDR:** use a rate from the device's fixed list, and check the antenna name
with `probe` (usually just `RX`).

```yaml
sdr:
  driver: "driver=rtlsdr"
  sample_rate: 1024000
  center_freq: 118250000
  gain: 40
  antenna: "RX"
```
