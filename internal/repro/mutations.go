package repro

import (
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

// PeakShapedTwin preserves a WAV's absolute peak level while deliberately
// altering its waveform. It exists so a drill can demonstrate the forbidden
// conclusion: a matching normalized peak with a different signal.
func PeakShapedTwin(wav []byte) ([]byte, error) {
	clip, _, err := audiocodec.DecodeWAV(wav)
	if err != nil {
		return nil, err
	}
	peak := 0.0
	for _, sample := range clip.Samples {
		if value := math.Abs(float64(sample)); value > peak {
			peak = value
		}
	}
	if peak == 0 {
		return wav, nil
	}
	// Normalize the shaper so the loudest sample still maps to the exact
	// original peak: the peak level is preserved, the waveform is not.
	scale := math.Tanh(1)
	samples := make([]float32, len(clip.Samples))
	for index, sample := range clip.Samples {
		samples[index] = float32(math.Tanh(float64(sample)/peak) / scale * peak)
	}
	return audiocodec.EncodePCM16(clip.SampleRate, clip.Channels, samples)
}

// ShiftSamples returns a WAV whose samples are offset by delta LSB while all
// geometry stays the same. It is a pure source-byte mutation.
func ShiftSamples(wav []byte, delta int) ([]byte, error) {
	clip, _, err := audiocodec.DecodeWAV(wav)
	if err != nil {
		return nil, err
	}
	shift := float32(delta) / 32768.0
	samples := make([]float32, len(clip.Samples))
	for index, sample := range clip.Samples {
		value := sample + shift
		if value > 1 {
			value = 1
		}
		if value < -1 {
			value = -1
		}
		samples[index] = value
	}
	return audiocodec.EncodePCM16(clip.SampleRate, clip.Channels, samples)
}
