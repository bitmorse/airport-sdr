// The embeddable player: one button, plus the postMessage protocol described
// in docs/embed.md.
//
// The audio comes from player.js, shared with the main page. This file is only
// the button, the state, and the message plumbing.
//
// The frame is same-origin with the receiver, so the low-latency websocket and
// the status API both work here exactly as they do on the main page. What the
// host page can do is limited to the commands below, deliberately: it cannot
// switch channel groups, because one tuner serves every listener.

(function () {
  'use strict';

  const PROTOCOL = 1;
  const body = document.body;

  const channel = body.dataset.channel;
  // Already checked against the allowlist by the server. Empty means the host
  // either sent no origin or sent one that is not permitted.
  const origin = body.dataset.origin || '';

  const button = document.getElementById('toggle');
  const icon = document.getElementById('icon');
  const squelch = document.getElementById('squelch');
  const message = document.getElementById('msg');
  const listeners = document.getElementById('listeners');

  document.getElementById('freq').textContent =
    (Number(body.dataset.frequency) / 1e6).toFixed(3) + ' MHz';
  if (body.dataset.theme !== 'auto') {
    document.documentElement.dataset.theme = body.dataset.theme;
  }

  // --- messaging ------------------------------------------------------------

  function emit(type, fields) {
    if (!origin) return; // nobody to talk to, and nowhere safe to post
    parent.postMessage(Object.assign(
      { source: 'airport-sdr', protocol: PROTOCOL, type }, fields || {}), origin);
  }

  function fail(code, text) {
    message.textContent = ' \u00b7 ' + text;
    emit('error', { code, message: text });
  }

  // --- the player -----------------------------------------------------------

  const player = AirportSDR.createPlayer({
    channel,
    onError: (code, text) => fail(code, text),
    onState: (state) => emit('state', state),
  });

  let muted = body.dataset.muted === '1';
  player.setMuted(muted);

  // A triangle when idle, a square when playing. Icons rather than words keep
  // the widget narrow enough to sit inside someone else's layout.
  const PLAY = 'M2 1l7 4-7 4z';
  const STOP = 'M2 2h6v6H2z';

  function render() {
    const playing = player.state().playing;
    icon.firstElementChild.setAttribute('d', playing ? STOP : PLAY);
    button.classList.toggle('on', playing);
    button.setAttribute('aria-label',
      playing ? 'Stop' : 'Listen' + (muted ? ' (muted)' : ''));
    button.title = playing ? (muted ? 'Playing, muted' : 'Stop') : 'Listen';
  }

  async function play() {
    await player.play();
    render();
  }

  function pause() {
    player.pause();
    render();
  }

  button.addEventListener('click', () => {
    player.state().playing ? pause() : play();
  });

  window.addEventListener('message', (event) => {
    // Only the origin this page was built for may command it.
    if (!origin || event.origin !== origin) return;
    const msg = event.data;
    if (!msg || msg.source !== 'airport-sdr') return;

    switch (msg.type) {
      case 'play':      play();                                   break;
      case 'pause':     pause();                                  break;
      case 'mute':      muted = true;  player.setMuted(true);  render(); break;
      case 'unmute':    muted = false; player.setMuted(false); render(); break;
      case 'setVolume': player.setVolume(msg.value);              break;
      case 'getState':  emit('state', player.state());            break;
      default: break;
    }
  });

  // --- channel state --------------------------------------------------------

  // Squelch and level come from the same status endpoint the main page uses.
  // It is same-origin inside the frame, so no CORS is involved.
  let lastOpen = null;
  let lastListeners = null;

  async function poll() {
    let status;
    try {
      status = await (await fetch('/api/status')).json();
    } catch (err) {
      return;
    }

    const state = (status.channels || []).find((c) => c.name === channel);
    if (!state) return;

    squelch.classList.toggle('open', state.squelch_open);
    if (state.squelch_open !== lastOpen) {
      lastOpen = state.squelch_open;
      emit('squelch', { open: state.squelch_open });
    }
    emit('level', { db: state.level_db });

    // How many people are on this channel, this one included. Emitted only on
    // change: it moves rarely, unlike the level.
    const count = state.listeners || 0;
    listeners.textContent = count;
    if (count !== lastListeners) {
      lastListeners = count;
      emit('listeners', { count });
    }

    // A channel in an idle group is never going to carry audio until the
    // receiver's operator switches to it. Say so rather than looking broken.
    if (status.active_group && body.dataset.group &&
        status.active_group !== body.dataset.group) {
      message.textContent = ' \u00b7 on ' + status.active_group;
    } else if (message.textContent) {
      message.textContent = '';
    }
  }

  // --- start ----------------------------------------------------------------

  render();
  poll();
  setInterval(poll, 1000);

  if (!origin && body.dataset.origin !== undefined && location.search.includes('origin=')) {
    fail('origin-not-allowed', 'this site is not permitted to embed the player');
  }

  // Nothing plays until someone asks: either a click on the button, or a play
  // command from the host page. The player never starts on its own.
  emit('ready', {
    channel,
    group: body.dataset.group,
    frequency: Number(body.dataset.frequency),
    audioRate: Number(body.dataset.audioRate),
  });
})();
