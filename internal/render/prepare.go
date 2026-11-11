package render

import (
	"context"
	"errors"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

type SourceInput struct {
	Asset domain.SourceAsset
	WAV   []byte
}

type preparedSpan struct {
	Index            int
	SourceID         string
	SourceDigest     string
	SourceStartFrame int64
	SourceEndFrame   int64
	OutputStartFrame int64
	OutputEndFrame   int64
	OutputFrames     int64
	CrossfadeFrames  int64
	GainDB           float64
	ChannelAction    string
	ChannelMatrix    []float64
	Pan              *float64
	ChannelSelect    string
	Clip             *audiocodec.Clip
}

func prepareSpans(
	ctx context.Context,
	plan domain.Timeline,
	sources map[string]SourceInput,
) ([]preparedSpan, int64, error) {
	decoded := make(map[string]*audiocodec.Clip, len(sources))
	prepared := make([]preparedSpan, 0, len(plan.Spans))
	cursor := int64(0)
	for index, span := range plan.Spans {
		source, ok := sources[span.SourceID]
		if !ok {
			return nil, 0, errors.New("timeline references a missing source")
		}
		clip, ok := decoded[span.SourceID]
		if !ok {
			parsed, _, err := audiocodec.DecodeWAV(source.WAV)
			if err != nil {
				return nil, 0, err
			}
			clip = parsed
			decoded[span.SourceID] = parsed
		}
		selected, err := audiocodec.SliceFrames(ctx, clip, span.SourceStartFrame, span.SourceEndFrame)
		if err != nil {
			return nil, 0, err
		}
		resampled, err := audiocodec.Resample(ctx, selected, plan.SampleRate)
		if err != nil {
			return nil, 0, err
		}
		var (
			mapped        *audiocodec.Clip
			action        string
			channelMatrix []float64
		)
		if span.Pan != nil || span.ChannelSelect != "" {
			if plan.OutputChannels != 2 {
				return nil, 0, errors.New("explicit channel routing requires a stereo output")
			}
			routed, routedAction, matrix, routeErr := audiocodec.ApplyChannelRoute(ctx, resampled, audiocodec.ChannelRoute{
				Pan:           span.Pan,
				ChannelSelect: span.ChannelSelect,
			})
			if routeErr != nil {
				return nil, 0, routeErr
			}
			mapped, action, channelMatrix = routed, routedAction, matrix
		} else {
			mapped, action, err = audiocodec.MapChannels(ctx, resampled, plan.OutputChannels)
			if err != nil {
				return nil, 0, err
			}
		}
		frames := mapped.FrameCount()
		if frames < 1 {
			return nil, 0, errors.New("span produced no output frames")
		}

		start := cursor
		if index > 0 {
			start -= span.CrossfadeFrames
		}
		if start < 0 {
			return nil, 0, errors.New("crossfade produced a negative output position")
		}
		end := start + frames
		cursor = end

		prepared = append(prepared, preparedSpan{
			Index:            index,
			SourceID:         span.SourceID,
			SourceDigest:     source.Asset.Digest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: start,
			OutputEndFrame:   end,
			OutputFrames:     frames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    action,
			ChannelMatrix:    channelMatrix,
			Pan:              span.Pan,
			ChannelSelect:    span.ChannelSelect,
			Clip:             mapped,
		})
	}
	return prepared, cursor, nil
}
