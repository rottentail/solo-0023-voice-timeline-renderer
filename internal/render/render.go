package render

import (
	"context"
	"errors"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

type Limits struct {
	MaxOutputFrames int64
}

type Result struct {
	WAV     []byte
	Receipt domain.RenderReceipt
	Output  domain.OutputAsset
}

func Render(
	ctx context.Context,
	renderID string,
	plan domain.Timeline,
	sources map[string]SourceInput,
	limits Limits,
	now time.Time,
) (Result, error) {
	return RenderWithTrace(ctx, renderID, plan, sources, limits, now, nil)
}

// RenderWithTrace behaves identically to Render but records the intermediate
// stage digests required to attribute a reproduction divergence to a layer.
func RenderWithTrace(
	ctx context.Context,
	renderID string,
	plan domain.Timeline,
	sources map[string]SourceInput,
	limits Limits,
	now time.Time,
	trace *Trace,
) (Result, error) {
	spans, totalFrames, err := prepareSpans(ctx, plan, sources, trace)
	if err != nil {
		return Result{}, err
	}
	if totalFrames < 1 {
		return Result{}, errors.New("timeline produced no output frames")
	}
	if limits.MaxOutputFrames > 0 && totalFrames > limits.MaxOutputFrames {
		return Result{}, errors.New("rendered timeline exceeds the configured frame limit")
	}
	mix, metrics, err := mixPrepared(ctx, spans, totalFrames, plan.OutputChannels)
	if err != nil {
		return Result{}, err
	}
	if trace != nil {
		trace.Mix = MixTrace{
			Frames:   totalFrames,
			Channels: plan.OutputChannels,
			Digest:   pcmDigest(mix),
			PeakDBFS: metrics.PeakDBFS,
			RMSDBFS:  metrics.RMSDBFS,
		}
	}
	normalized := normalizePeak(mix, plan.TargetPeakDBFS)
	if trace != nil {
		trace.Normalization = NormalizationTrace{
			BeforePeakDBFS: normalized.Before.PeakDBFS,
			AppliedGainDB:  normalized.GainDB,
			AfterPeakDBFS:  normalized.After.PeakDBFS,
			Digest:         pcmDigest(normalized.Samples),
		}
	}
	encoded, err := audiocodec.EncodePCM16(plan.SampleRate, plan.OutputChannels, normalized.Samples)
	if err != nil {
		return Result{}, err
	}
	receipt, output, err := buildReceipt(renderID, plan, encoded, normalized, spans, now)
	if err != nil {
		return Result{}, err
	}
	if trace != nil {
		trace.Encoding = EncodingTrace{
			SampleRate:    output.SampleRate,
			Channels:      output.Channels,
			BitsPerSample: output.BitsPerSample,
			Frames:        output.FrameCount,
			ByteCount:     output.ByteCount,
			Digest:        output.Digest,
		}
	}
	return Result{WAV: encoded, Receipt: receipt, Output: output}, nil
}
