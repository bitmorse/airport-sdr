# Running on Android with a USB SDR — feasibility

**Question:** can `airport-sdr` run on an Android phone or tablet with the SDR
plugged into its USB port?

**Short answer:** yes, and compute is not the obstacle — a modern phone SoC
dwarfs the Pi Zero 2 W this project already targets. The single deciding factor
is **USB access to the radio**, because Android is a Linux *kernel* but not a
GNU/Linux *userland*, and stock (unrooted) Android does not let an arbitrary
process open a USB device the way a Raspberry Pi does. Everything else — the Go
code, the DSP budget, the web server, the browser client — ports essentially for
free.

## What ports for free

The whole program is pure Go except for one file. `internal/sdr/soapy.go` is the
only place that imports `"C"`; it is the thin cgo translation layer over
SoapySDR, and CLAUDE.md already keeps it deliberately dumb for exactly this kind
of portability reason.

Both cross-compiles build today, with **no source changes**:

```bash
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build ./cmd/airport-sdr   # → /system/bin/linker64
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build ./cmd/airport-sdr   # → static, runs in Termux
```

The `android/arm64` binary is a native Android executable; the `linux/arm64`
binary is fully static and runs unchanged inside Termux. So a phone can run the
**server today** — but only in replay mode against a `.cf32` capture, because a
pure-Go build has no radio support (`soapy_stub.go` returns `ErrNoSoapy`). Live
reception needs the cgo SoapySDR path, and that is where the whole problem lives.

Non-obstacles worth stating plainly:

- **CPU.** The DSP measured 167 µs per 20 ms block (0.84% of one workstation
  core). A phone's application cores are far quicker than the Cortex-A53 the
  `hardware.md` budget is written against. Airband at 960 kS/s is trivial here.
- **Memory.** The zero-allocation audio path and the 256 MB `MemoryMax` in the
  systemd unit were sized for a 512 MB board. A phone has gigabytes.
- **Local audio latency.** Browsers treat `http://localhost` as a *secure
  context*, so a phone serving to its own browser at `http://localhost:8080`
  gets the low-latency `AudioWorklet`/WebSocket path **without any TLS** — the
  secure-context caveat in `deployment.md` only bites for LAN/remote addresses.
  For remote listening the Tailscale Android client works the same as on Linux.

## The one hard problem: USB on Android

On a Pi, SoapySDR's driver modules use libusb, libusb enumerates `/dev/bus/usb`
(usbfs), and the `plugdev` group grants access. **None of that is available to an
unrooted app on stock Android.** The USB device nodes are guarded by SELinux and
device-node ownership; a normal process cannot enumerate or open them. The only
sanctioned route is the Java **USB Host API** (`UsbManager`), which — after the
user taps "allow" on a permission dialog — hands your app a **file descriptor**
for the device.

libusb cannot do its usual discovery through that fd out of the box. The
established Android-SDR pattern (SDR++, SDRangel, RF Analyzer, and the RTL-SDR
Android driver) is:

1. Java/Kotlin obtains the fd from `UsbManager`.
2. Native code calls `libusb_set_option(LIBUSB_OPTION_NO_DEVICE_DISCOVERY)` and
   `libusb_wrap_sys_device(fd, …)` to use that fd directly.

That change lives **below** this project, in libusb and the driver module — not
in `airport-sdr` itself. Which route you take determines how much of it you have
to deal with.

## Three deployment paths

### Path A — Phone as remote display only (works today, zero new work)

Run the receiver on the Pi/edge device exactly as designed, put it on a tailnet,
open the browser client on the phone. This is not "SDR on the phone" — the radio
stays on the Pi — but it is the pragmatic baseline and needs nothing built.

### Path B — Rooted Android + Termux (lowest code effort, needs root)

With root, `/dev/bus/usb` becomes accessible and libusb enumerates normally, so
SoapySDR modules behave as on any Linux. The work is toolchain, not code:

- Build SoapySDR **and** the driver module for Android/bionic (via the NDK), then
  build `airport-sdr` with cgo against them — the equivalent of `make build-full`
  for `android/arm64`.
- Run under `tsu`/root so the process can reach the device node.
- Serve to the phone's own browser at `http://localhost:8080` (secure context,
  full low-latency path).

Fastest way to a *working* live receiver, at the cost of requiring a rooted
device and a bionic SoapySDR build the project does not ship.

### Path C — Native Android app (most work, no root, the real product)

The only path that runs on an unmodified phone. Shape:

- Package the Go DSP + web server as a `c-shared`/gomobile library inside an APK.
- Do USB in Kotlin via `UsbManager`; pass the fd into a libusb built with fd
  support plus the RTL driver, or read IQ from `librtlsdr` directly.
- Feed those samples in through a **new `sdr.Source` implementation** rather than
  `SoapySource`. This is where the existing architecture pays off: `Source` is
  the documented seam that already lets every later stage run against a
  `FileSource` with no radio. An "fd-fed" source slots in beside `SoapySource`
  and the DSP, receiver, stream and web layers need not change at all.

Largest lift; also the only one that ships to end users.

## Hardware and power caveats

- **Prefer RTL-SDR on a phone.** It is USB 2.0, low power, and has the most
  mature fd-based Android driver ecosystem. Airband needs only ~300 kHz of span,
  so RTL-SDR is more than enough — the project's tested **LimeSDR Mini is
  overkill here** and draws more current than many phone OTG ports supply. A
  LimeSDR/HackRF may need a **powered OTG hub**.
- The phone must support **USB OTG**, plus an OTG adapter/cable.
- USB bandwidth advice from `hardware.md` still applies: keep the sample rate
  low (960 kS/s), which is gentle on the bus and on power draw.

## Friction against the project's own rules

- **Build tags.** CLAUDE.md fixes the tag set at exactly two (`soapy`, `assert`).
  An Android hardware source would most naturally arrive as a third tag or a new
  build-tagged file — a deliberate decision for the maintainer, not an accident
  to slip in.
- **The cgo-stays-dumb rule.** Path C's fd-fed source keeps policy in pure Go
  (as `tune.go` already does) and confines the fd/libusb glue to a small
  untested translation layer, matching how `soapy.go` is treated today.

## Recommendation

If the goal is to *demonstrate* it on a phone soon and root is acceptable, take
**Path B**: it is a toolchain exercise with no code changes to this repo. If the
goal is a shippable app for unmodified phones, **Path C** is the real work, and
the right first step in this codebase is a new fd-fed `sdr.Source` behind the
existing interface — nothing else needs to move. **Path A** remains the
zero-effort answer whenever the radio is allowed to live on the edge device
rather than the phone.
