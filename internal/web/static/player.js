// The audio pipeline, shared by the main page and the embeddable player.
//
// Two paths, chosen automatically:
//
//   worklet  mu-law over WebSocket into an AudioWorklet. About 150 ms of
//            latency. Needs a secure context, so https or localhost.
//   wav      the open-ended WAV stream in a plain <audio> element. Works
//            anywhere, but lags by several seconds.
//
// Everything is exposed on window.AirportSDR rather than as a module, so a page
// can use it with a single <script> tag and no build step.

(function () {
  'use strict';

  // G.711 mu-law expansion. One byte per sample at 8 kHz is 64 kbit/s and needs
  // no codec support in the browser at all; this table is the whole decoder.
  const ULAW = (() => {
    const table = new Float32Array(256);
    for (let i = 0; i < 256; i++) {
      const u = ~i & 0xff;
      const exponent = (u >> 4) & 0x07;
      const mantissa = ((u & 0x0f) << 3) + 0x84;
      let sample = (mantissa << exponent) - 0x84;
      if (u & 0x80) sample = -sample;
      table[i] = sample / 32768;
    }
    return table;
  })();

  // AudioWorklet requires a secure context. Over plain http to a LAN address it
  // is unavailable, and the WAV fallback carries the audio instead.
  const canUseWorklet =
    window.isSecureContext && !!(window.AudioContext || window.webkitAudioContext);

  const noop = () => {};

  // createPlayer builds a player for one channel. Nothing happens until play().
  //
  //   channel   the channel name, as returned by /api/channels
  //   onBuffer  ({ buffered, underruns }) roughly twice a second, worklet only
  //   onError   (code, message) for anything the caller should surface
  //   onState   ({ playing, muted, volume, mode }) on every change
  function createPlayer(options) {
    const channel = options.channel;
    const onBuffer = options.onBuffer || noop;
    const onError = options.onError || noop;
    const onState = options.onState || noop;

    const mode = canUseWorklet ? 'worklet' : 'wav';
    let ctx = null, node = null, gain = null, socket = null, audio = null;
    let playing = false, muted = false, volume = 1;

    const state = () => ({ playing, muted, volume, mode });
    const announce = () => onState(state());

    function applyGain() {
      const level = muted ? 0 : volume;
      if (gain) gain.gain.value = level;
      if (audio) { audio.muted = muted; audio.volume = volume; }
    }

    async function startWorklet() {
      const AC = window.AudioContext || window.webkitAudioContext;
      // Asking for 8 kHz avoids resampling where the browser allows it; where
      // it does not, the worklet interpolates using the ratio below.
      ctx = new AC({ sampleRate: 8000, latencyHint: 'interactive' });
      await ctx.audioWorklet.addModule('/static/pcm-worklet.js');

      node = new AudioWorkletNode(ctx, 'pcm-player', {
        processorOptions: { ratio: 8000 / ctx.sampleRate, capacity: 16384, prebuffer: 480 },
      });
      node.port.onmessage = (e) => onBuffer(e.data);

      gain = ctx.createGain();
      node.connect(gain);
      gain.connect(ctx.destination);
      applyGain();

      const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
      socket = new WebSocket(
        `${scheme}://${location.host}/ws/audio/${encodeURIComponent(channel)}`);
      socket.binaryType = 'arraybuffer';

      socket.onmessage = (event) => {
        const bytes = new Uint8Array(event.data);
        const samples = new Float32Array(bytes.length);
        for (let i = 0; i < bytes.length; i++) samples[i] = ULAW[bytes[i]];
        node.port.postMessage(samples);
      };
      socket.onclose = () => {
        if (playing) { stop(); onError('stream-failed', 'the audio stream closed'); }
      };

      if (ctx.state === 'suspended') await ctx.resume();
    }

    async function startFallback() {
      audio = new Audio(`/stream/${encodeURIComponent(channel)}.wav`);
      applyGain();
      // Browsers refuse audible playback without user activation. Report it
      // rather than failing silently, so the caller can prompt for a click.
      await audio.play().catch((err) => {
        stop();
        onError('autoplay-blocked', String(err && err.message ? err.message : err));
      });
    }

    async function play() {
      if (playing) return;
      playing = true;
      try {
        await (canUseWorklet ? startWorklet() : startFallback());
        if (!canUseWorklet) onError('insecure-context', 'using the delayed WAV stream');
      } catch (err) {
        stop();
        onError('autoplay-blocked', String(err && err.message ? err.message : err));
        return;
      }
      announce();
    }

    function stop() {
      playing = false;
      if (socket) { socket.onclose = null; socket.close(); socket = null; }
      if (node) { node.disconnect(); node = null; }
      if (gain) { gain.disconnect(); gain = null; }
      if (ctx) { ctx.close(); ctx = null; }
      if (audio) { audio.pause(); audio.src = ''; audio = null; }
      announce();
    }

    return {
      play,
      pause: stop,
      setMuted(value) { muted = !!value; applyGain(); announce(); },
      setVolume(value) {
        volume = Math.max(0, Math.min(1, Number(value) || 0));
        applyGain();
        announce();
      },
      state,
      channel,
    };
  }

  window.AirportSDR = { createPlayer, canUseWorklet };
})();
