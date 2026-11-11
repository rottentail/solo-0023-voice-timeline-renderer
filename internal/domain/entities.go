package domain

import "time"

type SourceAsset struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Digest        string    `json:"digest"`
	ByteCount     int64     `json:"byte_count"`
	SampleRate    int       `json:"sample_rate"`
	Channels      int       `json:"channels"`
	BitsPerSample int       `json:"bits_per_sample"`
	FrameCount    int64     `json:"frame_count"`
	DurationMS    int64     `json:"duration_ms"`
	PeakDBFS      float64   `json:"peak_dbfs"`
	RMSDBFS       float64   `json:"rms_dbfs"`
	CreatedAt     time.Time `json:"created_at"`
}

// GainPoint is one breakpoint of a span's gain automation curve. Frame is
// expressed in the span's output (timeline sample-rate) frames, relative to the
// first frame of the span, so the curve keeps its timing after the source
// interval is resampled and cropped. GainDB is the automation gain at that
// frame; it is combined additively (in decibels) with the span's static
// GainDB.
type GainPoint struct {
	Frame  int64   `json:"frame"`
	GainDB float64 `json:"gain_db"`
}

// GainAutomation is a persistable piecewise-linear gain curve attached to a
// span. Interpolation always runs in decibel space ("linear_db"). A nil
// pointer means the span has no curve and keeps its fixed gain behavior.
type GainAutomation struct {
	Interpolation string      `json:"interpolation"`
	Points        []GainPoint `json:"points"`
}

type TimelineSpan struct {
	SourceID         string          `json:"source_id"`
	SourceStartFrame int64           `json:"source_start_frame"`
	SourceEndFrame   int64           `json:"source_end_frame"`
	GainDB           float64         `json:"gain_db"`
	CrossfadeFrames  int64           `json:"crossfade_frames"`
	GainAutomation   *GainAutomation `json:"gain_automation,omitempty"`
}

type Timeline struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	SampleRate     int            `json:"sample_rate"`
	OutputChannels int            `json:"output_channels"`
	TargetPeakDBFS float64        `json:"target_peak_dbfs"`
	Spans          []TimelineSpan `json:"spans"`
	State          string         `json:"state"`
	OutputFrames   int64          `json:"output_frames"`
	CreatedAt      time.Time      `json:"created_at"`
}

type RenderState string

const (
	RenderQueued     RenderState = "queued"
	RenderRendering  RenderState = "rendering"
	RenderFinalizing RenderState = "finalizing"
	RenderCompleted  RenderState = "completed"
	RenderFailed     RenderState = "failed"
)

type OutputAsset struct {
	Digest        string  `json:"digest"`
	ByteCount     int64   `json:"byte_count"`
	SampleRate    int     `json:"sample_rate"`
	Channels      int     `json:"channels"`
	BitsPerSample int     `json:"bits_per_sample"`
	FrameCount    int64   `json:"frame_count"`
	DurationMS    int64   `json:"duration_ms"`
	PeakDBFS      float64 `json:"peak_dbfs"`
	RMSDBFS       float64 `json:"rms_dbfs"`
}

type RenderJob struct {
	ID            string       `json:"id"`
	TimelineID    string       `json:"timeline_id"`
	State         RenderState  `json:"state"`
	Error         *ErrorDetail `json:"error,omitempty"`
	Output        *OutputAsset `json:"output,omitempty"`
	ReceiptDigest string       `json:"receipt_digest,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

// ReceiptGainPoint records one persisted automation breakpoint exactly as it
// was validated and applied, using output frames relative to the span start.
type ReceiptGainPoint struct {
	Frame       int64   `json:"frame"`
	GainDB      float64 `json:"gain_db"`
	EffectiveDB float64 `json:"effective_gain_db"`
}

// ReceiptGainAutomation makes the applied curve reproducible from the receipt:
// the breakpoints, the interpolation space, and the effective (static span
// gain plus automation gain) decibel extrema reached on the span.
type ReceiptGainAutomation struct {
	Interpolation      string             `json:"interpolation"`
	Points             []ReceiptGainPoint `json:"points"`
	EffectiveMinGainDB float64            `json:"effective_min_gain_db"`
	EffectiveMaxGainDB float64            `json:"effective_max_gain_db"`
}

type ReceiptSpan struct {
	Index            int                    `json:"index"`
	SourceID         string                 `json:"source_id"`
	SourceDigest     string                 `json:"source_digest"`
	SourceStartFrame int64                  `json:"source_start_frame"`
	SourceEndFrame   int64                  `json:"source_end_frame"`
	OutputStartFrame int64                  `json:"output_start_frame"`
	OutputEndFrame   int64                  `json:"output_end_frame"`
	OutputFrames     int64                  `json:"output_frames"`
	CrossfadeFrames  int64                  `json:"crossfade_frames"`
	GainDB           float64                `json:"gain_db"`
	ChannelAction    string                 `json:"channel_action"`
	GainAutomation   *ReceiptGainAutomation `json:"gain_automation,omitempty"`
}

type NormalizationReceipt struct {
	RequestedPeakDBFS float64 `json:"requested_peak_dbfs"`
	BeforePeakDBFS    float64 `json:"before_peak_dbfs"`
	AppliedGainDB     float64 `json:"applied_gain_db"`
	OutputPeakDBFS    float64 `json:"output_peak_dbfs"`
}

type RenderReceipt struct {
	SchemaVersion int                  `json:"schema_version"`
	RenderID      string               `json:"render_id"`
	TimelineID    string               `json:"timeline_id"`
	Output        OutputAsset          `json:"output"`
	Normalization NormalizationReceipt `json:"normalization"`
	Spans         []ReceiptSpan        `json:"spans"`
	Warnings      []string             `json:"warnings"`
	CreatedAt     time.Time            `json:"created_at"`
}

func ValidRenderTransition(from, to RenderState) bool {
	switch from {
	case RenderQueued:
		return to == RenderRendering || to == RenderFailed
	case RenderRendering:
		return to == RenderFinalizing || to == RenderFailed
	case RenderFinalizing:
		return to == RenderCompleted || to == RenderFailed
	default:
		return false
	}
}

func (job *RenderJob) Transition(to RenderState, detail *ErrorDetail, now time.Time) error {
	if job.State == to {
		return nil
	}
	if !ValidRenderTransition(job.State, to) {
		return E(
			KindConflict,
			"invalid_state_transition",
			"state",
			"render job cannot move from "+string(job.State)+" to "+string(to),
		)
	}
	if to == RenderFailed && detail == nil {
		return E(KindInternal, "missing_failure_detail", "error", "a failed render requires an error detail")
	}
	if to != RenderFailed {
		detail = nil
	}
	job.State = to
	job.Error = detail
	job.UpdatedAt = now.UTC()
	return nil
}
