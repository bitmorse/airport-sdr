# Deployment

## Installing

```bash
make install                          # /usr/local by default
make install PREFIX=/opt/airport-sdr  # elsewhere
make install DESTDIR=/tmp/stage       # stage into a package tree
```

Installs three things:

| | |
|---|---|
| `$(PREFIX)/bin/airport-sdr` | the binary |
| `/etc/airport-sdr/config.yaml` | **not overwritten if it already exists** |
| `/etc/systemd/system/airport-sdr.service` | the unit |

Then:

```bash
sudo useradd -r -G plugdev -s /usr/sbin/nologin airport-sdr
sudo systemctl enable --now airport-sdr
```

The service account needs `plugdev` for USB access to the radio. That is why the
unit does not use `DynamicUser` — it needs a stable identity in that group.

The unit is otherwise locked down: `ProtectSystem=strict`, `ProtectHome`,
`NoNewPrivileges`, a `SystemCallFilter`, and `MemoryMax=256M` so a runaway on a
512 MB board is killed rather than swapping the machine to death.

## Cross-compiling for an edge device

The pure-Go build needs no toolchain:

```bash
GOOS=linux GOARCH=arm64 make build     # Pi 3/4/5, Pi Zero 2 W
GOOS=linux GOARCH=arm GOARM=7 make build
```

`scp` the result and run it. This build has **no SDR support** — it works from a
recorded capture. For hardware you need cgo and therefore a cross toolchain with
`libsoapysdr-dev` for the target architecture, most easily through
`docker buildx --platform linux/arm64`, or simply by building on the device.

## Networking, and why loopback is the default

`server.listen` defaults to `127.0.0.1:8080`. Reaching the receiver from another
machine should be a deliberate act, so there are three options in order of
preference.

### Tailscale Serve — recommended

If the device is on a tailnet:

```bash
sudo tailscale serve --bg 8080
# → https://<device>.<tailnet>.ts.net/  proxying to 127.0.0.1:8080
```

This is the best option for more than convenience:

- **Real TLS certificate**, which the browser needs. `AudioWorklet` only runs in
  a secure context, so over plain `http://` to a LAN address the client silently
  falls back to the WAV endpoint and its multi-second delay. With https you get
  the ~150 ms WebSocket path.
- Works through NAT and CGNAT, common at remote sites.
- **No firewall port opens** and the process stays bound to loopback.
- Only tailnet devices can reach it. (`tailscale funnel` is the public variant —
  see the legal note below before considering it.)

To grant management without root once: `sudo tailscale set --operator=$USER`.

Verified working: WebSocket upgrades pass through the proxy, including the
same-origin check.

### SSH tunnel

For occasional access:

```bash
ssh -L 8080:localhost:8080 device
```

### Binding to all interfaces

```bash
airport-sdr --listen 0.0.0.0:8080 serve
```

This genuinely exposes the stream to the local network and may need a firewall
rule. You also lose the secure context, so browsers drop to the WAV fallback.
Prefer Tailscale.

## Endpoints

| Path | |
|---|---|
| `/` | Browser client |
| `/ws/audio/{name}` | µ-law over WebSocket, ~150 ms latency |
| `/stream/{name}.wav` | Open-ended WAV — VLC, `<audio>`, phone lock screens |
| `/api/channels` | Configured channels |
| `/api/status` | Level, squelch, listener count, dropped frames, active group |
| `/api/groups` | Configured tuner positions and which is live |
| `POST /api/groups/{name}/activate` | Retune to another group |
| `/embed/{name}` | Embeddable single-channel player; 404 unless an origin is allowed |
| `/oembed` | oEmbed discovery for an embed URL |

The WebSocket sends **nothing between transmissions** — the squelch already knows
when the channel is idle, so average bandwidth measured 17.8 kbit/s against
64 kbit/s continuous. A keepalive ping every 20 s stops intermediaries treating a
quiet channel as a dead connection.

The WAV endpoint stays continuous by contrast, because a gap there would stall
the player rather than simply sounding like silence.

## Monitoring

```bash
curl -s localhost:8080/api/status | jq
```

| Field | Watch for |
|---|---|
| `level_db` | Sudden drop = antenna or gain problem |
| `squelch_open` | Permanently true = `squelch_db` below the noise floor |
| `dropped` | Rising = a listener's connection cannot keep up (not a receiver fault) |
| `uptime_s` | Resets indicate the service is restarting |
| `active_group` | Which tuner position is live |

Note that switching groups retunes the one radio, so it changes what **every**
listener hears. That is inherent to a single-tuner receiver.

Overflow warnings in the log (`device dropped samples`) mean the host is not
keeping up with USB — lower the sample rate.

## Legal note

Receiving airband is generally unproblematic. *Redistributing* ATC audio is not
always so: in Switzerland telecom-secrecy rules (FMG) make public rebroadcast a
grey area, and other jurisdictions vary.

The defaults reflect this — loopback binding, and Tailscale Serve keeps the
stream inside your own tailnet. `tailscale funnel` and `--listen 0.0.0.0` are the
points at which that changes; check your local rules first.
