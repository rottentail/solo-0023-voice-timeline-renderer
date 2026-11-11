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

// Channel action labels reported through render receipts. They describe the
// exact channel operation applied while preparing a span for the mix.
const (
	ActionUnchanged    = "unchanged"
	ActionMonoToStereo = "mono_to_stereo"
	ActionStereoToMono = "stereo_to_mono"
	ActionPan          = "mono_pan"
	ActionSelectLeft   = "select_left"
	ActionSelectRight  = "select_right"
)

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
		return &Clip{SampleRate: clip.SampleRate, Channels: outputChannels, Samples: copied}, ActionUnchanged, nil
	}
	frames := clip.FrameCount()
	result := make([]float32, int(frames)*outputChannels)
	action := ActionMonoToStereo
	if outputChannels == 1 {
		action = ActionStereoToMono
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

// ChannelRoute is an explicit per-span channel decision. Exactly one of Pan or
// ChannelSelect is set; domain validation guarantees the setting matches the
// source layout and that the output is stereo.
type ChannelRoute struct {
	Pan           *float64
	ChannelSelect string
}

// ApplyChannelRoute turns a source clip into a stereo clip using an explicit
// pan or single-channel extraction. It returns the action label and the exact
// 2x2 gain matrix (rows are output L/R channels, columns are source L/R
// channels) so the render receipt can state the coefficients that were used.
//
// Pan uses an equal-power law: leftGain = cos(theta), rightGain = sin(theta)
// with theta = (pan+1) * pi/4. Every coefficient is computed once and reused,
// which keeps repeated renders bit-identical.
func ApplyChannelRoute(ctx context.Context, clip *Clip, route ChannelRoute) (*Clip, string, []float64, error) {
	if clip == nil || clip.Channels <= 0 {
		return nil, "", nil, errors.New("clip is empty")
	}
	if (route.Pan != nil) == (route.ChannelSelect != "") {
		return nil, "", nil, errors.New("channel route must set exactly one of pan or channel_select")
	}
	frames := clip.FrameCount()
	result := make([]float32, int(frames)*2)

	var action string
	var matrix [4]float64
	switch {
	case route.Pan != nil:
		if clip.Channels != 1 {
			return nil, "", nil, errors.New("pan requires a mono clip")
		}
		pan := *route.Pan
		theta := (pan + 1) * math.Pi / 4
		leftGain := math.Cos(theta)
		rightGain := math.Sin(theta)
		leftCoef := float32(leftGain)
		rightCoef := float32(rightGain)
		for frame := int64(0); frame < frames; frame++ {
			if frame%8192 == 0 {
				select {
				case <-ctx.Done():
					return nil, "", nil, ctx.Err()
				default:
				}
			}
			value := clip.Samples[int(frame)]
			result[int(frame)*2] = value * leftCoef
			result[int(frame)*2+1] = value * rightCoef
		}
		action = ActionPan
		matrix = [4]float64{leftGain, 0, 0, rightGain}
	case route.ChannelSelect == "left":
		if clip.Channels != 2 {
			return nil, "", nil, errors.New("channel_select left requires a stereo clip")
		}
		for frame := int64(0); frame < frames; frame++ {
			if frame%8192 == 0 {
				select {
				case <-ctx.Done():
					return nil, "", nil, ctx.Err()
				default:
				}
			}
			result[int(frame)*2] = clip.Samples[int(frame)*2]
		}
		action = ActionSelectLeft
		matrix = [4]float64{1, 0, 0, 0}
	case route.ChannelSelect == "right":
		if clip.Channels != 2 {
			return nil, "", nil, errors.New("channel_select right requires a stereo clip")
		}
		for frame := int64(0); frame < frames; frame++ {
			if frame%8192 == 0 {
				select {
				case <-ctx.Done():
					return nil, "", nil, ctx.Err()
				default:
				}
			}
			result[int(frame)*2+1] = clip.Samples[int(frame)*2+1]
		}
		action = ActionSelectRight
		matrix = [4]float64{0, 1, 0, 0}
	default:
		return nil, "", nil, errors.New("channel_select must be \"left\" or \"right\"")
	}
	mapped := &Clip{SampleRate: clip.SampleRate, Channels: 2, Samples: result}
	return mapped, action, matrix[:], nil
}
