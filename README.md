# airport-sdr

Listen to airband ATC in a browser. A single Go binary owns an SDR, demodulates
AM voice channels, and streams them to any browser on your network — built to
run unattended on small Linux boxes near an airfield.

```
SDR ──▶ frequency shift ──▶ decimate ──▶ AM demod ──▶ squelch ──▶ AGC ──▶ browser
```

## What it does

- **Receives AM airband voice** and serves it over WebSocket (~150 ms latency)
  or as a plain WAV stream that VLC, `<audio>` and phone lock screens can play.
- **Several channels from one capture.** Tower and Ground sit inside the same
  slice of spectrum, so a second channel costs config, not code or CPU.
- **Sends nothing between transmissions.** Airband is silent most of the time
  and the squelch already knows it, so the socket goes quiet — measured at
  17.8 kbit/s average against 64 kbit/s continuous, with no codec involved.
- **Diagnoses itself.** `probe` reports what the radio supports, `spectrum`
  draws what is actually on the air, and `replay` tells you which setting is
  wrong when a channel is silent.
- **Stays small.** ~3,800 lines, two dependencies, a 6.9 MB static binary that
  cross-compiles with no C toolchain unless you want hardware support.

## Requirements

| | |
|---|---|
| Go | 1.24+ (uses `t.Context`, `for range b.Loop`) |
| SoapySDR | `libsoapysdr-dev` + a driver module — **only for hardware builds** |
| Hardware | any SoapySDR-supported radio; see [docs/hardware.md](docs/hardware.md) |

The default build is pure Go and needs none of the above — it runs from a
recorded IQ file, which is how the DSP and browser client were developed.

```bash
# Debian/Ubuntu, only if you want to drive a radio
sudo apt install libsoapysdr-dev soapysdr-module-lms7   # or -rtlsdr, -airspy…
```

## Quick start

**Without a radio** — everything except the SDR itself:

```bash
make build
./bin/airport-sdr --config configs/config.yaml serve --iq your-capture.cf32
```

**With a radio:**

```bash
make build-full                       # needs libsoapysdr-dev
./bin/airport-sdr --config configs/config.yaml probe     # what does it support?
make record DURATION=60s              # capture IQ
make replay                           # demodulate to a WAV you can listen to
./bin/airport-sdr --config configs/config.yaml serve
```

Then open <http://localhost:8080>.

> Do `probe` and `record`/`replay` **before** `serve`. A receiver that hears
> nothing and a channel that happens to be quiet sound identical, and `replay`
> tells you which one you have. See [docs/hardware.md](docs/hardware.md).

## Tested hardware

| Radio | Status | Notes |
|---|---|---|
| **LimeSDR Mini v2.0** | ✅ working | `driver=lime`, antenna **LNAW**, gain **61** (its maximum). Verified receiving live tower audio at 118.100 MHz. |
| RTL-SDR | untested | Should work via `driver=rtlsdr`; rates are a fixed list, so pick one `probe` reports. |
| Airspy, HackRF, bladeRF | untested | Supported by SoapySDR; no reason to expect trouble. |

**The single most common failure is the antenna port.** A LimeSDR Mini offers
`NONE, LNAH, LNAL, LNAW, LB1, LB2`, and letting the driver choose gave a capture
64 dB below what LNAW produced. Always set `sdr.antenna` explicitly.

## Configuration

One YAML file, validated on load, unknown keys rejected. See
[docs/configuration.md](docs/configuration.md) for every field.

```yaml
sdr:
  driver: "driver=lime"
  sample_rate: 960000
  center_freq: 118250000    # NOT on a channel — see below
  gain: 61
  antenna: "LNAW"
channels:
  - name: Tower
    freq: 118100000
    mode: am
    squelch_db: -62         # measure this per site
```

Two rules the validator enforces, because both fail silently otherwise:

- **Never centre the tuner on a channel.** Most front ends have a DC spur at the
  local oscillator; here it measured 60 dB above the noise floor. Offset-tune and
  keep every channel at least 12.5 kHz away.
- **`sample_rate` must be a whole multiple of `audio.rate`.** The chain decimates
  by integers, so a ratio like 1.04 is refused rather than rounded.

## Commands

| Command | Purpose |
|---|---|
| `serve` | Run the receiver and web server (default). `--iq FILE` replays a capture. |
| `probe` | List the radio's antennas, gain range and supported rates. |
| `record` | Capture raw IQ to `.cf32`. |
| `replay` | Demodulate a capture to WAV and report why a channel was silent. |
| `spectrum` | Draw the captured span as a text chart, with max-hold. |
| `validate` | Check the config and exit. |

## Make targets

`make help` lists them. The pattern: **the default build is pure Go**, and
anything needing a C library lives behind a build tag, so
`GOOS=linux GOARCH=arm64 make build` just works.

| Target | |
|---|---|
| `build` | Pure-Go static binary — no cgo, cross-compiles anywhere |
| `build-full` | Adds `-tags soapy` for real SDR hardware |
| `test` | Full suite under `-race` |
| `test-alloc` | Asserts zero steady-state allocation in the audio path |
| `test-assert` | Runs with `-tags assert`, enabling runtime preconditions |
| `lint` | `go vet` plus golangci-lint, warnings are errors |
| `install` | Binary, config and systemd unit (honours `PREFIX`/`DESTDIR`) |
| `record` / `replay` / `bench` / `clean` | as named |

See [docs/development.md](docs/development.md) for the build tags and the
testing approach.

## Documentation

- **[docs/hardware.md](docs/hardware.md)** — tested radios, antenna and gain
  traps, and how to tell a deaf receiver from a quiet channel
- **[docs/configuration.md](docs/configuration.md)** — every config field and
  the rules the validator enforces
- **[docs/dsp.md](docs/dsp.md)** — the signal chain, why each stage exists, and
  the measurements behind the constants
- **[docs/development.md](docs/development.md)** — TDD workflow, build tags,
  coding rules, and developing with no radio attached
- **[docs/deployment.md](docs/deployment.md)** — installing on an edge device,
  systemd, and reaching it securely over Tailscale

## Status

Working end to end: verified receiving live ATC through a LimeSDR Mini and
playing it in a browser. 188 tests across every build mode.

Not done yet: ADPCM encoding (to halve the on-air bitrate), ARM cross-build
packaging, and measurement on real Pi-class hardware — the CPU figures quoted in
the docs are from a workstation.

## Legal note

Receiving airband is generally unproblematic. *Redistributing* ATC audio is not
always, and in Switzerland telecom-secrecy rules (FMG) make public rebroadcast a
grey area. The binary binds to loopback unless you pass `--listen` explicitly,
and [docs/deployment.md](docs/deployment.md) recommends Tailscale so the stream
stays private by default. Check your local rules before exposing it publicly.
