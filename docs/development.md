# Development

## The Makefile pattern

The organising rule: **the default build is pure Go**, and everything requiring a
C library sits behind a build tag. So this works with no toolchain setup:

```bash
GOOS=linux GOARCH=arm64 make build     # static binary for a Pi
```

and this needs `libsoapysdr-dev` but only when you actually want to drive a radio:

```bash
make build-full
```

### Build tags

Kept deliberately few (Power of 10 rule 8 adapted — Go has no preprocessor, so
the equivalent discipline is a minimal, documented tag set):

| Tag | Effect | Needs |
|---|---|---|
| `soapy` | Real SDR hardware via SoapySDR | `libsoapysdr-dev` + a driver module |
| `assert` | Compiles in runtime preconditions | — |

Without `soapy`, `NewSoapySource` returns an error telling you how to rebuild, so
the rest of the program carries no build tags of its own.

### Targets

| Target | Purpose |
|---|---|
| `build` | Pure-Go static binary → `bin/airport-sdr` |
| `build-full` | Adds `-tags soapy` |
| `test` | `go test -race ./...` |
| `watch` | Re-runs tests on save (needs `inotify-tools`) |
| `test-alloc` | The zero-allocation assertions |
| `test-assert` | Suite with `-tags assert` |
| `lint` | `go vet` + golangci-lint |
| `bench` | DSP benchmarks against the CPU budget |
| `record` / `replay` | Capture and demodulate (`DURATION=60s`) |
| `install` / `uninstall` | Honours `PREFIX` and `DESTDIR` |
| `clean` | Remove build output |

> `go build ./...` does **not** write `bin/`. Run `make build` before testing the
> binary, or you will run stale code — this has already produced one wrong
> conclusion during development.

## Developing without a radio

This is the normal case, and the reason `Source` is an interface. `FileSource`
replays a `.cf32` capture, so the DSP, the streaming layer and the browser client
can all be developed and regression-tested with nothing plugged in:

```bash
make record DURATION=60s                        # once, with hardware
./bin/airport-sdr serve --iq testdata/capture.cf32   # forever after
```

A capture is a **reproducible** signal. DSP changes can be compared against a
known result instead of against whatever happened to be on the air.

`.cf32` is interleaved little-endian float32 — the same format `rx_sdr` and GNU
Radio use, so captures interoperate.

## Red-green TDD

Write the failing test, confirm it fails *for the reason you expect*, then
implement. The DSP suits this unusually well: synthetic signals give exact
expected answers.

```go
// Transmit a known tone on a known offset, require the chain to hand it back.
in := modulatedChannel(fs/2, -150_000, 1_000, fs, 0.2, 0.8)
audio := ch.Process(in)
got := toneMagnitude(settled, 1000, 8000)   // want ~0.5 after AGC
```

**When a test passes first try, be suspicious.** Mutation-test it: break the code
deliberately and confirm the test notices. Flipping the NCO sign during
development dropped the recovered level from −12.8 to −91.5 dBFS and the tone
amplitude to 0.000 — that is what a test with teeth looks like.

Two things TDD cannot reach here, both marked in the code:

- **The cgo layer.** Policy (clamping, validation) lives in `tune.go`, pure Go and
  fully tested; the cgo file is thin translation. Keep that split.
- **Audio quality.** No test asserts "this sounds like a controller". The
  `record` → `replay` → listen loop is a human check.

## Coding rules

NASA's Power of 10, adapted to Go. The three that carry most of the weight:

**No allocation after initialisation.** The audio path allocates zero in steady
state, asserted by `TestAlloc*` tests. On a 512 MB device this is correctness,
not micro-optimisation.

**Functions under 60 lines**, enforced by `funlen`.

**Every return value checked.** `_ =` requires a comment explaining why.

### The assertion trap

`assert.That(cond, "...%v", x)` boxes `x` into `...any` **at the call site**, so
it allocates even when the assertion passes. Hence two functions:

```go
assert.That(cond, "constant message")        // hot paths — never allocates
assert.Thatf(cond, "formatted %v", value)    // constructors only
```

This is invisible in a normal build because assertions compile to nothing.
`make test-assert` is what catches it — and it did, on four hot-path assertions.

## Testing patterns worth copying

- **Chunk-invariance.** Stateful filters get a test proving the output does not
  depend on input block size. Block-boundary bugs sound almost right.
- **Never-blocks.** `stream.Hub` has a test that publishes 1000 frames to a
  subscriber that never reads, asserting `Publish` returns promptly — and another
  proving a slow listener does not degrade a fast one.
- **Allocation as a test**, not a benchmark note, so a regression fails CI.
- **Reference values.** G.711 encodings are pinned to the spec's constants, so a
  mistake shows up as a wrong constant rather than subtly distorted audio.

## Gotchas

- `pgrep -f "bin/airport-sdr"` matches the invoking shell's own command line and
  will kill your session. Use `fuser -k 8080/tcp` to stop a running server.
- Testing WebSockets with `curl` needs `--http1.1`; over HTTP/2 the upgrade
  headers are stripped and you get a 426.
- Cancelling a `conn.Read` via context timeout **closes** the connection in
  `coder/websocket`. To assert silence, use a long-lived reader goroutine and
  watch a channel.
- Captures are hundreds of megabytes and are gitignored. Keep them out.
