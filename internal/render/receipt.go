package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

func buildReceipt(
	renderID string,
	plan domain.Timeline,
	encoded []byte,
	normalized normalizationResult,
	spans []preparedSpan,
	now time.Time,
) (domain.RenderReceipt, domain.OutputAsset, error) {
	decoded, info, err := audiocodec.DecodeWAV(encoded)
	if err != nil {
		return domain.RenderReceipt{}, domain.OutputAsset{}, err
	}
	measurements := audiocodec.Measure(decoded.Samples)
	digest := sha256.Sum256(encoded)
	digestText := hex.EncodeToString(digest[:])
	output := domain.OutputAsset{
		Digest:        digestText,
		ByteCount:     int64(len(encoded)),
		SampleRate:    info.SampleRate,
		Channels:      info.Channels,
		BitsPerSample: info.BitsPerSample,
		FrameCount:    info.FrameCount,
		DurationMS:    framesToMilliseconds(info.FrameCount, info.SampleRate),
		PeakDBFS:      measurements.PeakDBFS,
		RMSDBFS:       measurements.RMSDBFS,
	}
	receiptSpans := make([]domain.ReceiptSpan, 0, len(spans))
	warnings := make([]string, 0)
	for _, span := range spans {
		receiptSpans = append(receiptSpans, domain.ReceiptSpan{
			Index:            span.Index,
			SourceID:         span.SourceID,
			SourceDigest:     span.SourceDigest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: span.OutputStartFrame,
			OutputEndFrame:   span.OutputEndFrame,
			OutputFrames:     span.OutputFrames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    span.ChannelAction,
		})
		if span.ChannelAction != "unchanged" {
			warnings = append(warnings, fmt.Sprintf("span %d applied %s", span.Index, span.ChannelAction))
		}
	}
	receipt := domain.RenderReceipt{
		SchemaVersion: 1,
		RenderID:      renderID,
		TimelineID:    plan.ID,
		Output:        output,
		Normalization: domain.NormalizationReceipt{
			RequestedPeakDBFS: plan.TargetPeakDBFS,
			BeforePeakDBFS:    normalized.Before.PeakDBFS,
			AppliedGainDB:     normalized.GainDB,
			OutputPeakDBFS:    measurements.PeakDBFS,
		},
		Spans:     receiptSpans,
		Warnings:  warnings,
		CreatedAt: now.UTC(),
	}
	return receipt, output, nil
}

func framesToMilliseconds(frames int64, sampleRate int) int64 {
	if frames <= 0 || sampleRate <= 0 {
		return 0
	}
	return int64(float64(frames) * 1000 / float64(sampleRate))
}
