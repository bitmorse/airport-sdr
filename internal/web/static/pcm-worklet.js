// Audio worklet that plays a live stream of 8 kHz samples arriving over a
// WebSocket.
//
// The problem it solves: audio arrives in bursts over a network, but the sound
// card wants a steady trickle. A ring buffer absorbs the jitter, and a
// fractional read pointer resamples from the receiver's 8 kHz to whatever rate
// the browser's audio context actually runs at.

class PCMPlayer extends AudioWorkletProcessor {
  constructor(options) {
    super();

    const opts = options.processorOptions || {};
    // Input samples consumed per output sample. 8000/48000 = 1/6.
    this.ratio = opts.ratio || 1;
    this.capacity = opts.capacity || 16384;
    // Wait for this much audio before starting, so ordinary network jitter
    // does not produce a gap in the very first word.
    this.prebuffer = opts.prebuffer || 480;

    this.buf = new Float32Array(this.capacity);
    this.written = 0;   // total samples ever written
    this.readPos = 0;   // fractional read position, same absolute scale
    this.playing = false;
    this.underruns = 0;
    this.reports = 0;

    this.port.onmessage = (event) => {
      if (event.data === 'reset') {
        this.written = this.readPos = 0;
        this.playing = false;
        return;
      }
      this.push(event.data);
    };
  }

  get available() {
    return this.written - this.readPos;
  }

  push(samples) {
    for (let i = 0; i < samples.length; i++) {
      this.buf[this.written % this.capacity] = samples[i];
      this.written++;
    }

    // If the consumer has fallen far behind, discard the oldest audio rather
    // than let the delay grow without bound. Stale ATC audio is worthless.
    if (this.available > this.capacity) {
      this.readPos = this.written - this.capacity;
    }
  }

  sampleAt(pos) {
    const i0 = Math.floor(pos);
    const frac = pos - i0;
    const a = this.buf[i0 % this.capacity];
    const b = this.buf[(i0 + 1) % this.capacity];
    return a + (b - a) * frac;
  }

  process(inputs, outputs) {
    const out = outputs[0][0];
    if (!out) return true;

    if (!this.playing && this.available >= this.prebuffer) {
      this.playing = true;
    }

    if (!this.playing) {
      out.fill(0);
      return true;
    }

    for (let i = 0; i < out.length; i++) {
      if (this.available < 2) {
        // Ran dry: emit silence and wait for the buffer to refill rather than
        // repeating stale samples, which sounds far worse than a short gap.
        out.fill(0, i);
        this.playing = false;
        this.underruns++;
        break;
      }
      out[i] = this.sampleAt(this.readPos);
      this.readPos += this.ratio;
    }

    // Report roughly twice a second so the page can show buffer health.
    if (++this.reports % 50 === 0) {
      this.port.postMessage({
        buffered: this.available / 8000,
        underruns: this.underruns,
      });
    }
    return true;
  }
}

registerProcessor('pcm-player', PCMPlayer);
