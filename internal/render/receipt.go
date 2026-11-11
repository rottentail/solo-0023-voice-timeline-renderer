package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
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
		receiptSpan := domain.ReceiptSpan{
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
		}
		if span.GainAutomation != nil {
			receiptSpan.GainAutomation = buildAutomationReceipt(span)
		}
		receiptSpans = append(receiptSpans, receiptSpan)
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

// buildAutomationReceipt turns the validated curve into receipt facts: every
// breakpoint with its effective decibel value (static span gain plus
// automation gain) and the effective extrema the curve reaches. With linear
// dB interpolation the extrema of a segment occur at its endpoints, so the
// breakpoint values describe the full gain envelope of the span.
func buildAutomationReceipt(span preparedSpan) *domain.ReceiptGainAutomation {
	curve := span.GainAutomation
	points := make([]domain.ReceiptGainPoint, 0, len(curve.Points))
	minimum := math.Inf(1)
	maximum := math.Inf(-1)
	for _, point := range curve.Points {
		effective := span.GainDB + point.GainDB
		if effective < minimum {
			minimum = effective
		}
		if effective > maximum {
			maximum = effective
		}
		points = append(points, domain.ReceiptGainPoint{
			Frame:       point.Frame,
			GainDB:      point.GainDB,
			EffectiveDB: effective,
		})
	}
	return &domain.ReceiptGainAutomation{
		Interpolation:      curve.Interpolation,
		Points:             points,
		EffectiveMinGainDB: minimum,
		EffectiveMaxGainDB: maximum,
	}
}
