package audiocodec

import (
	"errors"
	"math"
)

// WaveformBucket is a source-frame slice of a computed waveform envelope.
type WaveformBucket struct {
	StartFrame       int64
	EndFrame         int64
	OutputStartFrame int64
	OutputEndFrame   int64
	Min              float64
	Max              float64
}

// WaveformResult is the deterministic envelope for a source frame range.
// It describes decoded source samples only; no gain, normalization, or
// channel mapping is applied.
type WaveformResult struct {
	Buckets      []WaveformBucket
	OutputFrames int64
	Min          float64
	Max          float64
	PeakDBFS     float64
	Silent       bool
}

// ComputeWaveform builds an equal-time envelope for the half-open source frame
// range [startFrame, endFrame). The bucket boundaries are derived from the
// source range itself, which means every requested bucket covers the same
// fraction of time regardless of the target sample rate. The target rate only
// determines the output frame labels and OutputFrames, using the same rounding
// rule as timeline resampling.
//
// Min and Max are computed from every decoded sample in the bucket, across all
// channels. Interpolation is intentionally not used: the summary must expose
// the true source peaks rather than a resampled approximation.
func ComputeWaveform(
	clip *Clip,
	startFrame int64,
	endFrame int64,
	targetRate int,
	bucketCount int,
) (WaveformResult, error) {
	if clip == nil || clip.Channels <= 0 || clip.SampleRate <= 0 {
		return WaveformResult{}, errors.New("clip is empty")
	}
	frameCount := clip.FrameCount()
	if startFrame < 0 || endFrame <= startFrame || endFrame > frameCount {
		return WaveformResult{}, errors.New("frame range is outside the clip")
	}
	if targetRate < 8000 || targetRate > 192000 {
		return WaveformResult{}, errors.New("target sample rate is outside the accepted range")
	}
	rangeFrames := endFrame - startFrame
	if bucketCount < 1 || int64(bucketCount) > rangeFrames {
		return WaveformResult{}, errors.New("bucket count must fit inside the frame range")
	}

	outputFrames := int64(math.Round(float64(rangeFrames) * float64(targetRate) / float64(clip.SampleRate)))
	if outputFrames < 1 {
		outputFrames = 1
	}

	buckets := make([]WaveformBucket, bucketCount)
	minValue := math.Inf(1)
	maxValue := math.Inf(-1)
	for index := 0; index < bucketCount; index++ {
		start := startFrame + int64(index)*rangeFrames/int64(bucketCount)
		end := startFrame + int64(index+1)*rangeFrames/int64(bucketCount)
		bucket := WaveformBucket{
			StartFrame:       start,
			EndFrame:         end,
			OutputStartFrame: int64(index) * outputFrames / int64(bucketCount),
			OutputEndFrame:   int64(index+1) * outputFrames / int64(bucketCount),
			Min:              math.Inf(1),
			Max:              math.Inf(-1),
		}
		for frame := start; frame < end; frame++ {
			sampleStart := int(frame) * clip.Channels
			for channel := 0; channel < clip.Channels; channel++ {
				value := float64(clip.Samples[sampleStart+channel])
				if value < bucket.Min {
					bucket.Min = value
				}
				if value > bucket.Max {
					bucket.Max = value
				}
			}
		}
		if bucket.Min < minValue {
			minValue = bucket.Min
		}
		if bucket.Max > maxValue {
			maxValue = bucket.Max
		}
		buckets[index] = bucket
	}

	peak := math.Max(math.Abs(minValue), math.Abs(maxValue))
	return WaveformResult{
		Buckets:      buckets,
		OutputFrames: outputFrames,
		Min:          minValue,
		Max:          maxValue,
		PeakDBFS:     amplitudeDBFS(peak),
		Silent:       peak == 0,
	}, nil
}
