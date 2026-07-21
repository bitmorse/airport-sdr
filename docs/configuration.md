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

Device-level settings, shared by every group.

| Key | Type | Default | Notes |
|---|---|---|---|
| `driver` | string | `""` | SoapySDR device args, e.g. `driver=lime`. Naming the driver also stops SoapySDR probing every installed module, which is the source of the ALSA/SSDP error wall at startup. |
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

### `embed`

Who, if anyone, may frame a channel player. See [embed.md](embed.md).

| Key | Type | Default | Notes |
|---|---|---|---|
| `allowed_origins` | list | `[]` | Sites permitted to frame the player. Empty disables embedding: `/embed` returns 404. Each entry is a bare origin — `https://example.com`, not a page URL. The single entry `"*"` permits any site. |
| `width` | int | `280` | Default iframe width advertised by oEmbed. |
| `height` | int | `64` | Default iframe height. |

Framing the receiver puts another site's visitors on your radio, so it is opt-in
per origin rather than something a default turns on. The embedded player is
listen-only and cannot switch groups: one tuner serves everyone, so a visitor to
someone else's site must not be able to retune it.

### `groups[]`

One tuner position and the channels it covers. The radio serves one group at a
time; every channel inside it is demodulated in parallel, and switching groups
retunes the radio for **all** listeners.

| Key | Type | Notes |
|---|---|---|
| `name` | string | Required, unique. Shown in the UI and used by the switch API. |
| `center_freq` | Hz | Must not sit on any of its channels — see below. |
| `sample_rate` | Hz | **Must be a whole multiple of `audio.rate`.** Per-group, so a wide group can coexist with narrow ones. |
| `channels` | list | At least one. |

### `groups[].channels[]`

| Key | Type | Default | Notes |
|---|---|---|---|
| `name` | string | — | Required, and unique **across all groups**, since it keys the streaming endpoints. |
| `freq` | Hz | — | Required. Must be inside its group's span and clear of that group's LO. |
| `mode` | string | `am` | Only `am` today. |
| `squelch_db` | dBFS | `-35` | **Measure this per site.** See below. |

### The pre-groups form

A config with a flat top-level `channels` list plus `sdr.center_freq` and
`sdr.sample_rate` still works: it folds into a single group named `Default`.
Using both forms at once is refused, because which tuning applies would be
ambiguous.

## Rules the validator enforces

### The tuner must not sit on a channel

Direct-conversion front ends have a DC spur at the local oscillator — measured
here at 60 dB above the noise floor. A channel parked there is unreceivable, and
nothing downstream can recover it.

Every channel must be at least **12.5 kHz** from `center_freq`. Offset-tune:

```yaml
groups:
  - name: Tower
    center_freq: 118250000   # between the two channels, on neither
    sample_rate: 960000
    channels:
      - {name: Tower,  freq: 118100000, squelch_db: -62}   # -150 kHz
      - {name: Ground, freq: 118400000, squelch_db: -62}   # +150 kHz
```

### Channels must fit their group's captured span

A channel must be within ±40% of its group's sample rate from that group's centre — the outer edges
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

**LimeSDR Mini, an airfield across three tuner positions.** The eight channels
span 7.85 MHz, far more than one capture can hold, so they are clustered:

```yaml
sdr:
  driver: "driver=lime"
  gain: 61
  antenna: "LNAW"
audio:
  rate: 8000
server:
  listen: "127.0.0.1:8080"
groups:
  - name: Tower
    center_freq: 118250000
    sample_rate: 960000
    channels:
      - {name: Tower, freq: 118100000, squelch_db: -62}
  - name: Ground
    center_freq: 121805000
    sample_rate: 960000
    channels:
      - {name: Apron S,  freq: 121755000, squelch_db: -62}
      - {name: Apron N,  freq: 121855000, squelch_db: -62}
      - {name: Ground,   freq: 121902000, squelch_db: -62}
      - {name: Delivery, freq: 121925000, squelch_db: -62}
  - name: Approach
    center_freq: 125640000
    sample_rate: 960000
    channels:
      - {name: Approach,  freq: 125325000, squelch_db: -62}
      - {name: ATIS,      freq: 125725000, squelch_db: -62}
      - {name: Departure, freq: 125950000, squelch_db: -62}
```

**RTL-SDR:** use a rate from the device's fixed list, and check the antenna name
with `probe` (usually just `RX`).

```yaml
sdr:
  driver: "driver=rtlsdr"
  gain: 40
  antenna: "RX"
groups:
  - name: Tower
    center_freq: 118250000
    sample_rate: 1024000
    channels:
      - {name: Tower, freq: 118100000, squelch_db: -62}
```

An RTL-SDR tops out near 2.4 MS/s, so it cannot cover a whole airfield in one
capture. Grouping is the only route there — and it retunes faster than a
LimeSDR, which recalibrates on every tune.
