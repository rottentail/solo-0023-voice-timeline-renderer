package service

import (
	"context"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

const (
	// canonicalWaveformBuckets is the persisted full-range envelope size. It
	// is a cache, not a source fact; callers may request any bucket count that
	// matches the requested frame range, and a miss is rebuilt from immutable
	// media bytes.
	canonicalWaveformBuckets = 1024
	defaultWaveformBuckets   = 256
	maxWaveformBuckets       = 4096
)

// WaveformQuery carries optional read parameters. Nil fields inherit values
// from the immutable source metadata. StartFrame and EndFrame are half-open
// source frame bounds.
type WaveformQuery struct {
	StartFrame  *int64
	EndFrame    *int64
	SampleRate  *int
	BucketCount *int64
}

// GetWaveform returns a deterministic high/low peak envelope for a registered
// source. It never mutates source metadata, never clamps an out-of-range
// request, and never applies gain or normalization.
func (s *Service) GetWaveform(ctx context.Context, id string, query WaveformQuery) (domain.WaveformSummary, error) {
	if err := ctx.Err(); err != nil {
		return domain.WaveformSummary{}, err
	}
	if err := domain.ValidateResourceID("source_id", id); err != nil {
		return domain.WaveformSummary{}, err
	}

	asset, err := s.repo.GetSourceMeta(id)
	if err != nil {
		return domain.WaveformSummary{}, err
	}

	start := int64(0)
	if query.StartFrame != nil {
		start = *query.StartFrame
	}
	end := asset.FrameCount
	if query.EndFrame != nil {
		end = *query.EndFrame
	}
	targetRate := asset.SampleRate
	if query.SampleRate != nil {
		targetRate = *query.SampleRate
	}

	if start < 0 || end < 0 {
		return domain.WaveformSummary{}, domain.E(
			domain.KindInvalid,
			"invalid_frame_range",
			"start_frame",
			"frame range must start at or after frame zero",
		)
	}
	if start >= asset.FrameCount || end > asset.FrameCount {
		return domain.WaveformSummary{}, domain.E(
			domain.KindInvalid,
			"waveform_range_out_of_bounds",
			"end_frame",
			"frame range must remain inside the source frame count",
		)
	}
	if end <= start {
		return domain.WaveformSummary{}, domain.E(
			domain.KindInvalid,
			"invalid_frame_range",
			"start_frame",
			"frame range must be non-empty",
		)
	}
	if targetRate < 8000 || targetRate > 192000 {
		return domain.WaveformSummary{}, domain.E(
			domain.KindInvalid,
			"invalid_sample_rate",
			"sample_rate",
			"target sample rate must be between 8000 and 192000",
		)
	}

	rangeFrames := end - start
	bucketCount := int64(defaultWaveformBuckets)
	if query.BucketCount != nil {
		bucketCount = *query.BucketCount
		if bucketCount < 1 || bucketCount > maxWaveformBuckets || bucketCount > rangeFrames {
			return domain.WaveformSummary{}, domain.E(
				domain.KindInvalid,
				"invalid_bucket_count",
				"buckets",
				"bucket count must be between 1 and the smaller of 4096 and the source frame range",
			)
		}
	} else if bucketCount > rangeFrames {
		bucketCount = rangeFrames
	}

	// Reading media also re-verifies its digest, so a corrupted source is
	// reported as an integrity failure instead of being summarised silently.
	asset, payload, err := s.repo.GetSource(id)
	if err != nil {
		return domain.WaveformSummary{}, err
	}

	if reader, ok := s.repo.(sourceWaveformReader); ok {
		if canonical, found := reader.GetSourceWaveform(asset); found && canonicalSummaryValid(canonical, asset) {
			if start == 0 && end == asset.FrameCount && canonical.BucketCount%int(bucketCount) == 0 {
				if derived, deriveErr := deriveWaveformSummary(asset, canonical, targetRate, int(bucketCount)); deriveErr == nil {
					return derived, nil
				}
			}
		}
	}

	clip, info, decodeErr := audiocodec.DecodeWAV(payload)
	if decodeErr != nil {
		return domain.WaveformSummary{}, domain.Wrap(
			domain.KindInternal,
			"source_decode_failed",
			"source_id",
			"stored source media cannot be decoded",
			decodeErr,
		)
	}
	if info.SampleRate != asset.SampleRate ||
		info.Channels != asset.Channels ||
		info.BitsPerSample != asset.BitsPerSample ||
		info.FrameCount != asset.FrameCount {
		return domain.WaveformSummary{}, domain.E(
			domain.KindInternal,
			"source_integrity_error",
			"source_id",
			"decoded source facts do not match the stored metadata",
		)
	}
	result, computeErr := audiocodec.ComputeWaveform(clip, start, end, targetRate, int(bucketCount))
	if computeErr != nil {
		return domain.WaveformSummary{}, domain.Wrap(
			domain.KindInternal,
			"waveform_computation_failed",
			"source_id",
			"waveform summary could not be computed",
			computeErr,
		)
	}
	return waveformSummaryFromResult(asset, start, end, targetRate, result), nil
}

// canonicalWaveformSummary builds the persisted full-range cache. It is
// derived from the same decoded samples already measured during ingestion, so
// it cannot disagree with the source facts and does not add a second decode.
func canonicalWaveformSummary(asset domain.SourceAsset, clip *audiocodec.Clip) *domain.WaveformSummary {
	bucketCount := canonicalWaveformBuckets
	if asset.FrameCount < int64(bucketCount) {
		bucketCount = int(asset.FrameCount)
	}
	if bucketCount < 1 {
		return nil
	}
	result, err := audiocodec.ComputeWaveform(clip, 0, asset.FrameCount, asset.SampleRate, bucketCount)
	if err != nil {
		return nil
	}
	summary := waveformSummaryFromResult(asset, 0, asset.FrameCount, asset.SampleRate, result)
	return &summary
}

func waveformSummaryFromResult(
	asset domain.SourceAsset,
	startFrame, endFrame int64,
	targetRate int,
	result audiocodec.WaveformResult,
) domain.WaveformSummary {
	buckets := make([]domain.WaveformBucket, len(result.Buckets))
	for index, bucket := range result.Buckets {
		buckets[index] = domain.WaveformBucket{
			StartFrame:       bucket.StartFrame,
			EndFrame:         bucket.EndFrame,
			OutputStartFrame: bucket.OutputStartFrame,
			OutputEndFrame:   bucket.OutputEndFrame,
			Min:              bucket.Min,
			Max:              bucket.Max,
		}
	}
	return domain.WaveformSummary{
		SchemaVersion:    1,
		SourceID:         asset.ID,
		SourceDigest:     asset.Digest,
		SourceSampleRate: asset.SampleRate,
		SourceFrameCount: asset.FrameCount,
		SourcePeakDBFS:   asset.PeakDBFS,
		StartFrame:       startFrame,
		EndFrame:         endFrame,
		SampleRate:       targetRate,
		OutputFrames:     result.OutputFrames,
		BucketCount:      len(buckets),
		Silent:           result.Silent,
		Min:              result.Min,
		Max:              result.Max,
		PeakDBFS:         result.PeakDBFS,
		Buckets:          buckets,
	}
}

func deriveWaveformSummary(
	asset domain.SourceAsset,
	canonical domain.WaveformSummary,
	targetRate int,
	bucketCount int,
) (domain.WaveformSummary, error) {
	factor := canonical.BucketCount / bucketCount
	if factor < 1 || canonical.BucketCount%bucketCount != 0 {
		return domain.WaveformSummary{}, domain.E(
			domain.KindInternal,
			"waveform_cache_mismatch",
			"",
			"persisted waveform summary cannot be divided into the requested bucket count",
		)
	}
	outputFrames := domain.ScaledFrames(asset.FrameCount, asset.SampleRate, targetRate)
	if outputFrames < 1 {
		outputFrames = 1
	}
	buckets := make([]domain.WaveformBucket, bucketCount)
	minValue := math.Inf(1)
	maxValue := math.Inf(-1)
	for index := 0; index < bucketCount; index++ {
		chunk := canonical.Buckets[index*factor : (index+1)*factor]
		bucket := domain.WaveformBucket{
			StartFrame:       chunk[0].StartFrame,
			EndFrame:         chunk[len(chunk)-1].EndFrame,
			OutputStartFrame: int64(index) * outputFrames / int64(bucketCount),
			OutputEndFrame:   int64(index+1) * outputFrames / int64(bucketCount),
			Min:              chunk[0].Min,
			Max:              chunk[0].Max,
		}
		for _, source := range chunk[1:] {
			if source.Min < bucket.Min {
				bucket.Min = source.Min
			}
			if source.Max > bucket.Max {
				bucket.Max = source.Max
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
	return domain.WaveformSummary{
		SchemaVersion:    1,
		SourceID:         asset.ID,
		SourceDigest:     asset.Digest,
		SourceSampleRate: asset.SampleRate,
		SourceFrameCount: asset.FrameCount,
		SourcePeakDBFS:   asset.PeakDBFS,
		StartFrame:       0,
		EndFrame:         asset.FrameCount,
		SampleRate:       targetRate,
		OutputFrames:     outputFrames,
		BucketCount:      bucketCount,
		Silent:           peak == 0,
		Min:              minValue,
		Max:              maxValue,
		PeakDBFS:         audiocodec.LinearToDB(peak),
		Buckets:          buckets,
	}, nil
}

func canonicalSummaryValid(summary domain.WaveformSummary, asset domain.SourceAsset) bool {
	if summary.SchemaVersion != 1 ||
		summary.SourceID != asset.ID ||
		summary.SourceDigest != asset.Digest ||
		summary.SourceSampleRate != asset.SampleRate ||
		summary.SourceFrameCount != asset.FrameCount ||
		summary.SampleRate != asset.SampleRate ||
		summary.StartFrame != 0 ||
		summary.EndFrame != asset.FrameCount ||
		summary.BucketCount < 1 ||
		len(summary.Buckets) != summary.BucketCount {
		return false
	}
	expectedStart := int64(0)
	bucketMin := math.Inf(1)
	bucketMax := math.Inf(-1)
	for _, bucket := range summary.Buckets {
		if bucket.StartFrame != expectedStart || bucket.EndFrame <= bucket.StartFrame {
			return false
		}
		if math.IsNaN(bucket.Min) || math.IsInf(bucket.Min, 0) ||
			math.IsNaN(bucket.Max) || math.IsInf(bucket.Max, 0) ||
			bucket.Min > bucket.Max {
			return false
		}
		if bucket.Min < bucketMin {
			bucketMin = bucket.Min
		}
		if bucket.Max > bucketMax {
			bucketMax = bucket.Max
		}
		expectedStart = bucket.EndFrame
	}
	if expectedStart != asset.FrameCount || bucketMin != summary.Min || bucketMax != summary.Max {
		return false
	}
	// The cached aggregate peak is a restatement of the measured source fact.
	// If it drifted, rebuild instead of serving a different peak truth.
	peak := math.Max(math.Abs(summary.Min), math.Abs(summary.Max))
	if math.Abs(audiocodec.LinearToDB(peak)-asset.PeakDBFS) > 1e-9 {
		return false
	}
	return true
}
