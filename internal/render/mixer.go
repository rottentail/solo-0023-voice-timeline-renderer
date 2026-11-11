package render

import (
	"context"
	"errors"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

func mixPrepared(ctx context.Context, spans []preparedSpan, totalFrames int64, channels int) ([]float32, audiocodec.Metrics, error) {
	if totalFrames < 1 || channels < 1 {
		return nil, audiocodec.Metrics{}, errors.New("mix dimensions are invalid")
	}
	if totalFrames > int64(^uint(0)>>1)/int64(channels) {
		return nil, audiocodec.Metrics{}, errors.New("mix is too large for this process")
	}
	mix := make([]float32, int(totalFrames)*channels)
	for _, span := range spans {
		frames := span.Clip.FrameCount()
		// When a curve is present, evaluate one linear gain factor per output
		// frame once. Spans without a curve keep the single constant factor and
		// the exact original arithmetic, so legacy projects render identical
		// bytes.
		var frameGains []float32
		gain := float32(audiocodec.DBToLinear(span.GainDB))
		if span.GainAutomation != nil {
			frameGains = make([]float32, frames)
			for frame := int64(0); frame < frames; frame++ {
				totalDB := span.GainDB + automationGain(span.GainAutomation, frame)
				frameGains[frame] = float32(audiocodec.DBToLinear(totalDB))
			}
		}
		for frame := int64(0); frame < frames; frame++ {
			if frame%4096 == 0 {
				select {
				case <-ctx.Done():
					return nil, audiocodec.Metrics{}, ctx.Err()
				default:
				}
			}
			incoming := float32(1)
			outgoing := float32(1)
			if span.CrossfadeFrames > 0 && frame < span.CrossfadeFrames {
				progress := (float64(frame) + 0.5) / float64(span.CrossfadeFrames)
				angle := progress * math.Pi / 2
				incoming = float32(math.Sin(angle))
				outgoing = float32(math.Cos(angle))
			}
			frameGain := gain
			if frameGains != nil {
				frameGain = frameGains[frame]
			}
			inStart := int(frame) * channels
			outStart := int(span.OutputStartFrame+frame) * channels
			for channel := 0; channel < channels; channel++ {
				value := span.Clip.Samples[inStart+channel] * frameGain * incoming
				if span.CrossfadeFrames > 0 && frame < span.CrossfadeFrames {
					mix[outStart+channel] *= outgoing
				}
				mix[outStart+channel] += value
			}
		}
	}
	return mix, audiocodec.Measure(mix), nil
}
