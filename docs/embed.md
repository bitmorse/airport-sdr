# Embedding a channel in another site

airport-sdr can be embedded in another page as an iframe player, controlled by
the host page's JavaScript over `postMessage`.

The embedded player is deliberately compact — around 230x48 — so it can sit
inside someone else's layout without dominating it: a round play/stop button,
the channel name over its frequency, a carrier indicator and a listener count.
Everything else your page draws itself, from the events below.

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
  src="https://receiver.example/embed/Tower?origin=https://yoursite.example"
  width="230" height="48"
  allow="autoplay"
  style="border:0"
></iframe>
```

Three things matter:

- **`origin`** must be your page's exact origin (scheme + host + port). The
  player validates it against the receiver's allowlist and uses it as the
  `targetOrigin` for every message it sends. Without it the player still plays,
  but sends no messages.
- **`allow="autoplay"`** is required, and its name is misleading: it does *not*
  make anything play by itself. A click on your page is not user activation
  *inside* a cross-origin frame, so without this attribute the browser refuses
  your `play()` command. See [Starting playback](#starting-playback).
- **The receiver operator must allow your origin.** See
  [Server configuration](#server-configuration). Until then the frame will not
  render at all, and `/embed` returns 404.

## URL parameters

| Parameter | Values | Default | Meaning |
|---|---|---|---|
| `origin` | an origin | — | Your page's origin. Required for `postMessage`. |
| `muted` | `1`, `0` | `0` | Start muted, so your page can unmute later. |
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
| `listeners` | `count` | When the number of people listening to this channel changes, including you. |
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

## Starting playback

**The player never starts on its own.** Audio begins only when someone asks for
it: a click on the player's own button, or a `play` command from your page.
There is no autoplay parameter.

For your `play()` to work, two things must hold:

1. **`allow="autoplay"` on the iframe.** Despite the name, this is what lets a
   *cross-origin* frame play at all on your behalf — a click on your page does
   not count as user activation inside the frame. Without it, `play` is refused.
2. **A user gesture on your page first.** Browsers still require that a person
   has interacted with your page before audible playback starts.

So the reliable pattern is to call `play()` from a real click handler:

```js
document.getElementById('listen').addEventListener('click', () => atc.play());
```

If playback is refused you receive `error` with code `autoplay-blocked`. Retry
from a genuine click.

## Errors

| `code` | Meaning |
|---|---|
| `autoplay-blocked` | The browser refused playback, usually because no user gesture had happened yet, or `allow="autoplay"` is missing from the iframe. Retry from a real click. |
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
  "html": "<iframe src=\"https://receiver.example/embed/Tower\" width=\"230\" height=\"48\" allow=\"autoplay\" style=\"border:0\"></iframe>",
  "width": 230,
  "height": 48
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
  # Default iframe size advertised by the oEmbed endpoint. The player is
  # compact; an embedder can override the iframe size anyway.
  width: 230
  height: 48
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
