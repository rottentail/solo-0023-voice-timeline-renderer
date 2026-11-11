package domain

// WaveformBucket is one deterministic time slice of a source waveform
// summary. StartFrame and EndFrame are half-open source frame bounds.
// OutputStartFrame and OutputEndFrame are the corresponding half-open bounds
// after mapping the slice to the requested timeline sample rate.
//
// Min and Max are the exact lowest and highest decoded sample amplitudes in
// the slice, across every source channel. They are never normalized, gained,
// or clipped by the summary path.
type WaveformBucket struct {
	StartFrame       int64   `json:"start_frame"`
	EndFrame         int64   `json:"end_frame"`
	OutputStartFrame int64   `json:"output_start_frame"`
	OutputEndFrame   int64   `json:"output_end_frame"`
	Min              float64 `json:"min"`
	Max              float64 `json:"max"`
}

// WaveformSummary is the stable, client-independent envelope for a half-open
// source frame range. The source facts are copied from the immutable source
// metadata and are never rewritten by summary reads.
//
// SchemaVersion is 1. The zero value is not a valid summary.
type WaveformSummary struct {
	SchemaVersion    int              `json:"schema_version"`
	SourceID         string           `json:"source_id"`
	SourceDigest     string           `json:"source_digest"`
	SourceSampleRate int              `json:"source_sample_rate"`
	SourceFrameCount int64            `json:"source_frame_count"`
	SourcePeakDBFS   float64          `json:"source_peak_dbfs"`
	StartFrame       int64            `json:"start_frame"`
	EndFrame         int64            `json:"end_frame"`
	SampleRate       int              `json:"sample_rate"`
	OutputFrames     int64            `json:"output_frames"`
	BucketCount      int              `json:"bucket_count"`
	Silent           bool             `json:"silent"`
	Min              float64          `json:"min"`
	Max              float64          `json:"max"`
	PeakDBFS         float64          `json:"peak_dbfs"`
	Buckets          []WaveformBucket `json:"buckets"`
}
