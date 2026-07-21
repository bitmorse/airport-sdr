package stream

import "math/bits"

// mu-law companding, as specified by ITU-T G.711.
//
// This is the fallback audio codec, and the reason the browser client can be
// built and proven before any real codec exists. It halves the bitrate of
// 16-bit PCM to 64 kbit/s at 8 kHz while keeping a roughly constant
// signal-to-noise ratio across the whole dynamic range, which is what makes it
// so well suited to speech: quiet passages get fine steps, loud ones coarse.
//
// It is also trivial to decode in a dozen lines of JavaScript, so the browser
// needs no codec support at all.

const (
	// ulawBias is added before encoding so the segment calculation works on a
	// strictly positive value.
	ulawBias = 0x84
	// ulawClip is the largest magnitude the format can represent.
	ulawClip = 32635
)

// EncodeULaw compresses one 16-bit sample to a mu-law byte.
func EncodeULaw(pcm int16) byte {
	var sign byte
	if pcm < 0 {
		sign = 0x80
		// Negating the most negative value would overflow, so clamp first.
		if pcm == -32768 {
			pcm = -ulawClip
		}
		pcm = -pcm
	}
	if pcm > ulawClip {
		pcm = ulawClip
	}
	pcm += ulawBias

	// The exponent is the index of the highest set bit above the low 7 bits,
	// which is what gives the format its logarithmic segments.
	exponent := byte(bits.Len8(uint8((pcm>>7)&0xFF)) - 1)
	mantissa := byte((pcm >> (exponent + 3)) & 0x0F)

	return ^(sign | (exponent << 4) | mantissa)
}

// DecodeULaw expands a mu-law byte back to 16-bit. It exists mainly so tests
// can measure the round trip; the browser does this in JavaScript.
func DecodeULaw(u byte) int16 {
	u = ^u

	exponent := (u >> 4) & 0x07
	mantissa := int32(u&0x0F)<<3 + ulawBias
	sample := (mantissa << exponent) - ulawBias

	if u&0x80 != 0 {
		return int16(-sample)
	}
	return int16(sample)
}

// EncodeULawBlock converts normalised float samples straight to mu-law bytes,
// returning how many it wrote. It stops at whichever buffer runs out first and
// allocates nothing.
func EncodeULawBlock(dst []byte, samples []float32) int {
	n := len(samples)
	if len(dst) < n {
		n = len(dst)
	}
	for i := 0; i < n; i++ {
		dst[i] = EncodeULaw(toInt16(samples[i]))
	}
	return n
}
