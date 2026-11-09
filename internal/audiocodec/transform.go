package audiocodec

import (
	"context"
	"errors"
	"math"
)

func SliceFrames(ctx context.Context, clip *Clip, startFrame, endFrame int64) (*Clip, error) {
	if clip == nil || clip.Channels <= 0 {
		return nil, errors.New("clip is empty")
	}
	frameCount := clip.FrameCount()
	if startFrame < 0 || endFrame <= startFrame || endFrame > frameCount {
		return nil, errors.New("frame interval is outside the clip")
	}
	start := int(startFrame) * clip.Channels
	end := int(endFrame) * clip.Channels
	result := make([]float32, end-start)
	copy(result, clip.Samples[start:end])
	return &Clip{SampleRate: clip.SampleRate, Channels: clip.Channels, Samples: result}, nil
}

func Resample(ctx context.Context, clip *Clip, targetRate int) (*Clip, error) {
	if clip == nil || clip.Channels <= 0 {
		return nil, errors.New("clip is empty")
	}
	if targetRate < 8000 || targetRate > 192000 {
		return nil, errors.New("target sample rate is outside the accepted range")
	}
	sourceFrames := clip.FrameCount()
	targetFrames := int64(math.Round(float64(sourceFrames) * float64(targetRate) / float64(clip.SampleRate)))
	if targetFrames < 1 {
		targetFrames = 1
	}
	if sourceFrames == int64(targetFrames) && clip.SampleRate == targetRate {
		copied := make([]float32, len(clip.Samples))
		copy(copied, clip.Samples)
		return &Clip{SampleRate: targetRate, Channels: clip.Channels, Samples: copied}, nil
	}
	result := make([]float32, int(targetFrames)*clip.Channels)
	step := float64(clip.SampleRate) / float64(targetRate)
	for frame := int64(0); frame < targetFrames; frame++ {
		if frame%4096 == 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}
		position := float64(frame) * step
		left := int64(position)
		if left >= sourceFrames-1 {
			left = sourceFrames - 1
		}
		right := left + 1
		if right >= sourceFrames {
			right = sourceFrames - 1
		}
		weight := position - float64(left)
		for channel := 0; channel < clip.Channels; channel++ {
			first := clip.Samples[int(left)*clip.Channels+channel]
			second := clip.Samples[int(right)*clip.Channels+channel]
			result[int(frame)*clip.Channels+channel] = first + (second-first)*float32(weight)
		}
	}
	return &Clip{SampleRate: targetRate, Channels: clip.Channels, Samples: result}, nil
}

func MapChannels(ctx context.Context, clip *Clip, outputChannels int) (*Clip, string, error) {
	if clip == nil || clip.Channels <= 0 {
		return nil, "", errors.New("clip is empty")
	}
	if outputChannels != 1 && outputChannels != 2 {
		return nil, "", errors.New("output channel count must be one or two")
	}
	if clip.Channels == outputChannels {
		copied := make([]float32, len(clip.Samples))
		copy(copied, clip.Samples)
		return &Clip{SampleRate: clip.SampleRate, Channels: outputChannels, Samples: copied}, "unchanged", nil
	}
	frames := clip.FrameCount()
	result := make([]float32, int(frames)*outputChannels)
	action := "mono_to_stereo"
	if outputChannels == 1 {
		action = "stereo_to_mono"
	}
	for frame := int64(0); frame < frames; frame++ {
		if frame%8192 == 0 {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			default:
			}
		}
		if clip.Channels == 1 && outputChannels == 2 {
			value := clip.Samples[int(frame)]
			result[int(frame)*2] = value
			result[int(frame)*2+1] = value
			continue
		}
		left := clip.Samples[int(frame)*2]
		right := clip.Samples[int(frame)*2+1]
		result[int(frame)] = (left + right) * 0.5
	}
	return &Clip{SampleRate: clip.SampleRate, Channels: outputChannels, Samples: result}, action, nil
}
