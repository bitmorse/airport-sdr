# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

A Go binary that owns an SDR, AM-demodulates airband voice channels, and streams
them to browsers. The real deployment target is **small Linux edge devices
(Pi Zero 2 W class)** near an airfield. The development workstation is not the
target — do not let its capacity influence design decisions.

## Build and test

```bash
make build          # pure Go, no cgo — the default and the cross-compile path
make build-full     # -tags soapy, needs libsoapysdr-dev
make test           # -race, the gate that must stay green
make test-assert    # -tags assert: runtime preconditions compiled in
make test-alloc     # zero-allocation assertions for the audio path
make lint           # go vet + golangci-lint, warnings are errors
```

**Always run `make test` and `make test-assert`.** They catch different things —
see the allocation rule below.

`go build ./...` does **not** write `bin/`. Use `make build` before running the
binary, or you will test stale code. This has already caused one wrong
conclusion ("the feature does nothing" when the feature was simply not built).

## Development method

**Red-green TDD.** Write the failing test, confirm it fails *for the expected
reason*, then implement. The DSP is pure functions over slices and synthetic
signals give exact expected answers, so this is genuinely practical here.

Two places TDD does not reach, and they are marked as such in the code:

- **The cgo Soapy layer** (`internal/sdr/soapy.go`). Policy — clamping and
  validation — lives in `tune.go`, which is pure Go and fully tested. The cgo
  file is a thin translation layer. Keep it that way.
- **Audio quality.** No test asserts "this sounds like a controller". The
  `record` → `replay` → listen loop is a human check.

When a test passes on the first run, be suspicious. Mutation-test the claim:
break the thing deliberately and confirm the test catches it.

## Coding rules (NASA Power of 10, adapted to Go)

| Rule | How it applies here | Enforced by |
|---|---|---|
| No recursion, no `goto` | — | review, lint |
| Bounded loops | Goroutine event loops are the documented exception; they must be `ctx`-cancellable | review |
| **No allocation after init** | The audio path allocates **zero** in steady state | `TestAlloc*` |
| Functions ≤ 60 lines | | `funlen` |
| ≥2 assertions per function | Preconditions plus `internal/assert` | `make test-assert` |
| Check every return value | `_ =` requires a comment saying why | `errcheck` |
| Limit build tags | Exactly two: `soapy`, `assert` | review |
| Static analysis on | warnings are errors | `make lint` |

### The allocation rule bites in a non-obvious way

`assert.That(cond, "...%v", x)` boxes `x` into `...any` **at the call site**, so
it allocates even when the assertion passes. That is why the package has two
functions:

- `assert.That(cond, "constant message")` — no formatting, no allocation. **Use
  this in any per-sample or per-block path.**
- `assert.Thatf(cond, "format %v", x)` — allocates; constructors and setup only.

This only shows up under `-tags assert`, which is why `make test-assert` exists.

## Architecture

```
cmd/airport-sdr/     CLI: serve, probe, record, replay, spectrum, validate
internal/config/     YAML config + validation (the gate before hardware)
internal/sdr/        Source interface, file replay, cgo SoapySDR, tune policy
internal/dsp/        NCO, FIR, AM demod, AGC, squelch, decimation planner, FFT
internal/receiver/   the receive loop, group switching, DSP chain lifecycle
internal/stream/     mu-law, WAV, and the listener fan-out hub
internal/web/        HTTP + WebSocket, embedded browser client
```

Dependencies are deliberately few: `coder/websocket` and `yaml.v3`. Think hard
before adding a third — "as light as possible" is an explicit project goal.

### Invariants worth preserving

- **`Source` is the seam that makes everything testable without a radio.**
  `FileSource` replays a `.cf32` capture; every later stage was built against it.
  Do not add code paths that only work with hardware attached.
- **Publishing audio must never block.** `stream.Hub` drops frames for a slow
  listener rather than stalling the receive chain. Tests assert this.
- **Status is read through atomics**, not a mutex, so HTTP handlers never
  contend with the DSP goroutine.
- **Switching groups is a sequential handoff**, never shared mutable state:
  stop the stream, drain it, retune, restart. `Source.Retune` must never be
  called while a stream is live, and a receiver test asserts it is not.
- **The web package knows nothing about DSP types** — it takes a hub and a state
  function. That is what lets it be tested without building a receive chain.

## Facts learned the hard way

Do not re-derive these:

- **Antenna port is the most common failure.** A LimeSDR Mini offers
  `NONE, LNAH, LNAL, LNAW, LB1, LB2`; the driver's default gave a capture 64 dB
  weaker than `LNAW`. Airband wants **LNAW at gain 61** (its maximum).
- **The DC spur is real and large** — measured 60 dB above the noise floor at the
  local oscillator. Offset-tune always; config enforces a 12.5 kHz minimum.
- **Squelch thresholds are site-specific.** The working value here (`-62 dBFS`)
  sits in an 8 dB window between a −67 noise floor and −59 peaks. Never hardcode
  a threshold; `replay` prints the numbers needed to choose one.
- **AGC needs a lot of headroom.** Real signals arrive near the noise floor;
  `MaxGain` is 4000 because 200 left genuine traffic 13 dB too quiet.
- **cgo pointer rules:** `readStream` gets a **C-allocated** buffer. Passing a Go
  array containing Go pointers violates the cgo rules and trips `cgocheck`.
- **`AudioWorklet` requires a secure context.** Over plain `http://` to a LAN
  address the browser client falls back to the WAV endpoint. Serve over https —
  Tailscale Serve does this for free.
- **WebSocket needs HTTP/1.1.** Testing with `curl` over HTTP/2 returns 426
  because h2 strips `Connection: Upgrade`. Use `--http1.1`.

## Shell gotchas in this repo

- `pgrep -f "bin/airport-sdr"` matches the invoking shell's own command line and
  will kill your session. Use `fuser -k 10000/tcp` to stop a running server.
- Captures are hundreds of megabytes. They are gitignored; keep it that way.
