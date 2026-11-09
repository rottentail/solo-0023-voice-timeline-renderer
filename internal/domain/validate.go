package domain

import (
	"math"
	"regexp"
	"strings"
)

var resourceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{2,63}$`)

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
		if math.IsNaN(span.GainDB) || math.IsInf(span.GainDB, 0) || span.GainDB < -36 || span.GainDB > 12 {
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
