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
	Clip             *audiocodec.Clip
}

func prepareSpans(
	ctx context.Context,
	plan domain.Timeline,
	sources map[string]SourceInput,
	trace *Trace,
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
			if trace != nil {
				trace.Sources = append(trace.Sources, SourceTrace{
					SourceID:      span.SourceID,
					BytesDigest:   bytesDigest(source.WAV),
					DecodedFrames: parsed.FrameCount(),
					SampleRate:    parsed.SampleRate,
					Channels:      parsed.Channels,
					PCMDigest:     pcmDigest(parsed.Samples),
				})
			}
		}
		selected, err := audiocodec.SliceFrames(ctx, clip, span.SourceStartFrame, span.SourceEndFrame)
		if err != nil {
			return nil, 0, err
		}
		resampled, err := audiocodec.Resample(ctx, selected, plan.SampleRate)
		if err != nil {
			return nil, 0, err
		}
		mapped, action, err := audiocodec.MapChannels(ctx, resampled, plan.OutputChannels)
		if err != nil {
			return nil, 0, err
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

		if trace != nil {
			trace.Spans = append(trace.Spans, SpanTrace{
				Index:             index,
				SourceID:          span.SourceID,
				SourceStartFrame:  span.SourceStartFrame,
				SourceEndFrame:    span.SourceEndFrame,
				TargetSampleRate:  plan.SampleRate,
				ResampledFrames:   resampled.FrameCount(),
				ResampledChannels: resampled.Channels,
				ResampledDigest:   pcmDigest(resampled.Samples),
				ChannelAction:     action,
				MappedFrames:      frames,
				MappedDigest:      pcmDigest(mapped.Samples),
				OutputStartFrame:  start,
				OutputFrames:      frames,
				CrossfadeFrames:   span.CrossfadeFrames,
				GainDB:            span.GainDB,
				GainedDigest:      gainedDigest(mapped.Samples, span.GainDB),
			})
		}
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
			Clip:             mapped,
		})
	}
	return prepared, cursor, nil
}
