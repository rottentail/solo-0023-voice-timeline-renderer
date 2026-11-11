package domain

import (
	"math"
	"regexp"
	"strings"
)

var resourceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{2,63}$`)

// Gain limits shared by static span gains and automation breakpoints.
const (
	MinGainDB         = -36.0
	MaxGainDB         = 12.0
	MaxGainPoints     = 256
	GainInterpolation = "linear_db"
)

type SourceFacts struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	FrameCount    int64
}

func ValidateResourceID(field, value string) error {
	if !resourceIDPattern.MatchString(value) {
		return E(
			KindInvalid,
			"invalid_id",
			field,
			"identifier must be 3-64 characters using letters, digits, underscore, or hyphen",
		)
	}
	return nil
}

func ValidateName(value string) error {
	value = strings.TrimSpace(value)
	if len(value) < 1 || len(value) > 120 {
		return E(KindInvalid, "invalid_name", "name", "name must contain 1-120 characters")
	}
	return nil
}

func ValidateSourceFacts(field string, facts SourceFacts) error {
	if facts.SampleRate < 8000 || facts.SampleRate > 192000 {
		return E(KindInvalid, "invalid_sample_rate", field+".sample_rate", "sample rate must be between 8000 and 192000")
	}
	if facts.Channels != 1 && facts.Channels != 2 {
		return E(KindInvalid, "invalid_channels", field+".channels", "source channels must be mono or stereo")
	}
	if facts.BitsPerSample != 16 {
		return E(KindInvalid, "invalid_sample_format", field+".bits_per_sample", "source samples must use 16-bit integer PCM")
	}
	if facts.FrameCount < 1 {
		return E(KindInvalid, "empty_source", field+".frame_count", "source must contain at least one frame")
	}
	return nil
}

func ValidateTimeline(plan Timeline, sources map[string]SourceAsset, maxSpans int, maxOutputFrames int64) error {
	if err := ValidateResourceID("timeline_id", plan.ID); err != nil {
		return err
	}
	if err := ValidateName(plan.Name); err != nil {
		return err
	}
	if plan.SampleRate < 8000 || plan.SampleRate > 192000 {
		return E(KindInvalid, "invalid_sample_rate", "sample_rate", "timeline sample rate must be between 8000 and 192000")
	}
	if plan.OutputChannels != 1 && plan.OutputChannels != 2 {
		return E(KindInvalid, "invalid_channels", "output_channels", "output channels must be one or two")
	}
	if plan.TargetPeakDBFS < -24 || plan.TargetPeakDBFS > -0.1 {
		return E(KindInvalid, "invalid_target_peak", "target_peak_dbfs", "target peak must be between -24 and -0.1 dBFS")
	}
	if len(plan.Spans) < 1 || len(plan.Spans) > maxSpans {
		return E(KindInvalid, "invalid_span_count", "spans", "timeline span count is outside the allowed range")
	}

	cursor := int64(0)
	previousFrames := int64(0)
	for index, span := range plan.Spans {
		field := "spans[" + intString(index) + "]"
		source, ok := sources[span.SourceID]
		if !ok {
			return E(KindInvalid, "source_not_found", field+".source_id", "referenced source does not exist")
		}
		if span.SourceStartFrame < 0 || span.SourceEndFrame <= span.SourceStartFrame || span.SourceEndFrame > source.FrameCount {
			return E(
				KindInvalid,
				"invalid_source_interval",
				field+".source_end_frame",
				"source interval must be non-empty and remain inside the source frame count",
			)
		}
		if math.IsNaN(span.GainDB) || math.IsInf(span.GainDB, 0) || span.GainDB < MinGainDB || span.GainDB > MaxGainDB {
			return E(KindInvalid, "invalid_gain", field+".gain_db", "gain must be finite and between -36 and 12 dB")
		}
		if span.CrossfadeFrames < 0 {
			return E(KindInvalid, "invalid_crossfade", field+".crossfade_frames", "crossfade cannot be negative")
		}
		if index == 0 && span.CrossfadeFrames != 0 {
			return E(KindInvalid, "invalid_crossfade", field+".crossfade_frames", "the first span cannot crossfade")
		}

		outputFrames := ScaledFrames(span.SourceEndFrame-span.SourceStartFrame, source.SampleRate, plan.SampleRate)
		if outputFrames < 1 {
			return E(KindInvalid, "empty_span_output", field+".source_end_frame", "source interval becomes empty at the timeline sample rate")
		}
		if err := validateGainAutomation(field, span, outputFrames); err != nil {
			return err
		}
		if index > 0 {
			if span.CrossfadeFrames > previousFrames || span.CrossfadeFrames > outputFrames {
				return E(
					KindInvalid,
					"invalid_crossfade",
					field+".crossfade_frames",
					"crossfade cannot exceed either neighboring span duration",
				)
			}
			cursor -= span.CrossfadeFrames
		}
		cursor += outputFrames
		previousFrames = outputFrames
		if cursor > maxOutputFrames {
			return E(KindTooLarge, "timeline_too_long", "spans", "timeline output exceeds the configured frame limit")
		}
	}
	return nil
}

func ScaledFrames(frameCount int64, sourceRate, targetRate int) int64 {
	if frameCount <= 0 || sourceRate <= 0 || targetRate <= 0 {
		return 0
	}
	return int64(math.Round(float64(frameCount) * float64(targetRate) / float64(sourceRate)))
}

// validateGainAutomation enforces every automation constraint explicitly.
// Invalid input is rejected with a stable error instead of being silently
// clamped or normalized, so a persisted timeline always matches the request.
// Curve frames use the span's output timeline (post-resample) coordinates,
// ranging from 0 to outputFrames inclusive.
func validateGainAutomation(field string, span TimelineSpan, outputFrames int64) error {
	curve := span.GainAutomation
	if curve == nil {
		return nil
	}
	curveField := field + ".gain_automation"
	if curve.Interpolation != GainInterpolation {
		return E(
			KindInvalid,
			"invalid_automation_interpolation",
			curveField+".interpolation",
			"gain automation interpolation must be \""+GainInterpolation+"\"",
		)
	}
	if len(curve.Points) < 2 {
		return E(
			KindInvalid,
			"invalid_automation_curve",
			curveField+".points",
			"gain automation curve must contain at least two points",
		)
	}
	if len(curve.Points) > MaxGainPoints {
		return E(
			KindInvalid,
			"invalid_automation_curve",
			curveField+".points",
			"gain automation curve cannot contain more than 256 points",
		)
	}
	previousFrame := int64(-1)
	for index, point := range curve.Points {
		pointField := curveField + ".points[" + intString(index) + "]"
		if point.Frame < 0 || point.Frame > outputFrames {
			return E(
				KindInvalid,
				"invalid_automation_time_range",
				pointField+".frame",
				"automation point frame must remain within the span output frame range",
			)
		}
		if point.Frame <= previousFrame {
			return E(
				KindInvalid,
				"invalid_automation_time_range",
				pointField+".frame",
				"automation point frames must be strictly increasing",
			)
		}
		previousFrame = point.Frame
		if math.IsNaN(point.GainDB) || math.IsInf(point.GainDB, 0) {
			return E(KindInvalid, "invalid_automation_gain", pointField+".gain_db", "automation gain must be a finite number of decibels")
		}
		if point.GainDB < MinGainDB || point.GainDB > MaxGainDB {
			return E(KindInvalid, "invalid_automation_gain", pointField+".gain_db", "automation gain must be between -36 and 12 dB")
		}
		effective := span.GainDB + point.GainDB
		if effective < MinGainDB || effective > MaxGainDB {
			return E(
				KindInvalid,
				"invalid_automation_gain",
				pointField+".gain_db",
				"combined span and automation gain must remain between -36 and 12 dB",
			)
		}
	}
	if curve.Points[0].Frame != 0 {
		return E(
			KindInvalid,
			"invalid_automation_endpoint",
			curveField+".points[0].frame",
			"gain automation curve must start at frame 0 of the span",
		)
	}
	if curve.Points[len(curve.Points)-1].Frame != outputFrames {
		return E(
			KindInvalid,
			"invalid_automation_endpoint",
			curveField+".points["+intString(len(curve.Points)-1)+"].frame",
			"gain automation curve must end at the last output frame of the span",
		)
	}
	return nil
}

func TimelineOutputFrames(plan Timeline, sources map[string]SourceAsset) int64 {
	total := int64(0)
	for _, span := range plan.Spans {
		source, ok := sources[span.SourceID]
		if !ok {
			return 0
		}
		total += ScaledFrames(span.SourceEndFrame-span.SourceStartFrame, source.SampleRate, plan.SampleRate)
		total -= span.CrossfadeFrames
	}
	return total
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[position:])
}
