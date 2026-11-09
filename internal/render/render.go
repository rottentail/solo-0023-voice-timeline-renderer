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
	spans, totalFrames, err := prepareSpans(ctx, plan, sources)
	if err != nil {
		return Result{}, err
	}
	if totalFrames < 1 {
		return Result{}, errors.New("timeline produced no output frames")
	}
	if limits.MaxOutputFrames > 0 && totalFrames > limits.MaxOutputFrames {
		return Result{}, errors.New("rendered timeline exceeds the configured frame limit")
	}
	mix, _, err := mixPrepared(ctx, spans, totalFrames, plan.OutputChannels)
	if err != nil {
		return Result{}, err
	}
	normalized := normalizePeak(mix, plan.TargetPeakDBFS)
	encoded, err := audiocodec.EncodePCM16(plan.SampleRate, plan.OutputChannels, normalized.Samples)
	if err != nil {
		return Result{}, err
	}
	receipt, output, err := buildReceipt(renderID, plan, encoded, normalized, spans, now)
	if err != nil {
		return Result{}, err
	}
	return Result{WAV: encoded, Receipt: receipt, Output: output}, nil
}
