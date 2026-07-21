# Embedding a channel in another site

airport-sdr can be embedded in another page as an iframe player, controlled by
the host page's JavaScript over `postMessage`.

The embedded player is deliberately minimal: a listen/stop button, a carrier
indicator and the channel's name and frequency. Everything else your page draws
itself, from the `squelch` and `level` events below.

## Simplest option, if you only need play/pause/mute

You may not need any of this. The WAV endpoint is a plain audio stream, and
media elements load cross-origin without CORS:

```html
<audio id="atc" src="https://receiver.example/stream/Tower.wav"></audio>
<script>
  const atc = document.getElementById('atc');
  atc.play();  atc.pause();  atc.muted = true;  atc.volume = 0.4;
</script>
```

Direct, synchronous, no protocol. The trade is a few seconds of latency and no
squelch or level information. Use the iframe below when you want the ~150 ms
path and live channel state.

## Quick start

```html
<iframe
  src="https://receiver.example/embed/Tower?origin=https://yoursite.example&muted=1"
  width="280" height="64"
  allow="autoplay"
  style="border:0"
></iframe>
```

Three things matter:

- **`origin`** must be your page's exact origin (scheme + host + port). The
  player validates it against the receiver's allowlist and uses it as the
  `targetOrigin` for every message it sends. Without it the player still plays,
  but sends no messages.
- **`allow="autoplay"`** delegates your page's autoplay permission into the
  frame. Without it, programmatic `play()` is refused.
- **The receiver operator must allow your origin.** See
  [Server configuration](#server-configuration). Until then the frame will not
  render at all, and `/embed` returns 404.

## URL parameters

| Parameter | Values | Default | Meaning |
|---|---|---|---|
| `origin` | an origin | — | Your page's origin. Required for `postMessage`. |
| `muted` | `1`, `0` | `0` | Start muted. Required in practice if you want autoplay. |
| `autoplay` | `1`, `0` | `0` | Attempt playback on load. Browsers only permit this while muted. |
| `theme` | `dark`, `light`, `auto` | `auto` | Follows the viewer's system setting by default. |

The path segment is the channel name, URL-encoded: `/embed/Apron%20S`.
An unknown channel returns **404**.

## Controlling the player

Your page cannot touch the frame's DOM — that is the same-origin policy, and it
is what keeps the arrangement safe. Control is by message passing.

```js
const frame  = document.getElementById('atc');
const RECEIVER = 'https://receiver.example';

function send(message) {
  frame.contentWindow.postMessage({ source: 'airport-sdr', ...message }, RECEIVER);
}
```

Every message in both directions carries `source: "airport-sdr"` and
`protocol: 1`. Ignore anything without them: browser extensions and frameworks
post plenty of unrelated traffic to `window`.

### Commands you send

| `type` | Fields | Notes |
|---|---|---|
| `play` | — | Subject to autoplay policy; see below. |
| `pause` | — | |
| `mute` | — | |
| `unmute` | — | Requires user activation if audio has not started. |
| `setVolume` | `value` 0…1 | |
| `getState` | — | Prompts a `state` event. |

### Events you receive

| `type` | Fields | When |
|---|---|---|
| `ready` | `channel`, `group`, `frequency`, `audioRate` | Once, when the player has loaded. Do not send commands before this. |
| `state` | `playing`, `muted`, `volume` | On any change, and in reply to `getState`. |
| `squelch` | `open` | When the channel opens or closes, i.e. when a transmission starts or ends. |
| `level` | `db` | Roughly once a second, the channel's signal level in dBFS. |
| `error` | `code`, `message` | See [Errors](#errors). |

```js
window.addEventListener('message', (event) => {
  if (event.origin !== RECEIVER) return;
  const msg = event.data;
  if (msg?.source !== 'airport-sdr') return;

  switch (msg.type) {
    case 'ready':   console.log('playing', msg.channel, msg.frequency); break;
    case 'squelch': indicator.classList.toggle('live', msg.open);       break;
    case 'level':   meter.value = msg.db;                               break;
    case 'error':   console.warn(msg.code, msg.message);                break;
  }
});
```

### A small wrapper

Commands sent before `ready` are lost, so queue them:

```js
function airportSDR(frame, receiver) {
  let ready = false;
  const queue = [];

  const post = (m) => frame.contentWindow.postMessage(
    { source: 'airport-sdr', protocol: 1, ...m }, receiver);

  const send = (m) => ready ? post(m) : queue.push(m);

  window.addEventListener('message', (e) => {
    if (e.origin !== receiver || e.data?.source !== 'airport-sdr') return;
    if (e.data.type === 'ready') {
      ready = true;
      queue.splice(0).forEach(post);
    }
  });

  return {
    play:   () => send({ type: 'play' }),
    pause:  () => send({ type: 'pause' }),
    mute:   () => send({ type: 'mute' }),
    unmute: () => send({ type: 'unmute' }),
    volume: (v) => send({ type: 'setVolume', value: v }),
  };
}

const atc = airportSDR(document.getElementById('atc'), 'https://receiver.example');
atc.mute();
```

## Autoplay

Browsers refuse to start audible playback without user interaction, and this
applies whether you use the iframe or a bare `<audio>` element.

What works reliably:

1. Load with `muted=1&autoplay=1`. Muted playback is permitted.
2. Call `unmute()` from a real click handler on your page.

`allow="autoplay"` on the iframe is still required — it delegates your page's
permission into the frame. Without it even muted autoplay is refused.

If playback is blocked you receive `error` with code `autoplay-blocked`.

## Errors

| `code` | Meaning |
|---|---|
| `autoplay-blocked` | The browser refused playback. Retry from a user gesture. |
| `origin-not-allowed` | The `origin` parameter is not in the receiver's allowlist. No further messages are sent. |
| `stream-failed` | The connection to the receiver dropped. The player retries on its own. |
| `insecure-context` | The page was loaded over plain http, so the low-latency path is unavailable. The player falls back to the WAV stream automatically; audio still works but lags by a few seconds. |

## Discovery (oEmbed)

The receiver exposes an [oEmbed](https://oembed.com) endpoint, so platforms that
support it (WordPress, Discourse, Notion and others) can embed a pasted link
without any markup from you:

```
GET https://receiver.example/oembed?url=https://receiver.example/embed/Tower&format=json
```

```json
{
  "version": "1.0",
  "type": "rich",
  "provider_name": "airport-sdr",
  "title": "Tower — 118.100 MHz",
  "html": "<iframe src=\"https://receiver.example/embed/Tower\" width=\"280\" height=\"64\" allow=\"autoplay\" style=\"border:0\"></iframe>",
  "width": 280,
  "height": 64
}
```

Each embed page advertises this with a discovery link, so consumers find it
automatically. Only `format=json` is supported; `format=xml` returns 501.

## Server configuration

Embedding is **disabled by default**. The receiver's operator must list your
origin in its config:

```yaml
embed:
  # Origins permitted to frame the player. Empty disables embedding entirely.
  allowed_origins:
    - "https://yoursite.example"
  # Default iframe size advertised by the oEmbed endpoint. The player is a
  # single button, so it is small; an embedder can override it anyway.
  width: 280
  height: 64
```

This drives two things: the `Content-Security-Policy: frame-ancestors` header on
the embed response, and the `origin` values the player will accept and post to.
An origin that is not listed gets `error` code `origin-not-allowed`, and the
browser refuses to render the frame at all.

`allowed_origins: ["*"]` permits any site to embed the player. That is
appropriate for a deliberately public receiver and inappropriate otherwise.

## Limitations worth knowing

**The embedded player is listen-only.** It cannot switch channel groups. One
tuner serves every listener, so allowing an embed to retune would let any
visitor to your site take the receiver off frequency for everyone. If a channel
you embed is in a group that is not currently tuned, the player reports
`squelch: false` and stays silent until the receiver's operator switches to it.

**The receiver must be reachable by your visitors.** A receiver published only
to a private network — a Tailscale tailnet, for instance — is not reachable from
a public page, however the embed is configured.

**One channel per frame.** Embed several frames for several channels; they share
the receiver's audio path and cost it nothing extra.
