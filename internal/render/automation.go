package render

import "voice-timeline-renderer/internal/domain"

// automationGain evaluates a validated gain automation curve at frame, which
// is expressed in output (timeline-rate) frames relative to the start of the
// owning span. Interpolation is performed linearly in decibel space between
// neighboring breakpoints. The returned value is the automation gain only;
// the span's static gain is added by the caller.
//
// Validation guarantees at least two points, strictly increasing frames, and
// endpoints covering the whole span, so every frame in [0, lastFrame] lands
// inside a defined segment and no division by zero is possible.
func automationGain(curve *domain.GainAutomation, frame int64) float64 {
	if curve == nil || len(curve.Points) == 0 {
		return 0
	}
	points := curve.Points
	if frame <= points[0].Frame {
		return points[0].GainDB
	}
	last := len(points) - 1
	if frame >= points[last].Frame {
		return points[last].GainDB
	}
	// frames are strictly increasing and frame is strictly inside the range
	for index := 0; index < last; index++ {
		left := points[index]
		right := points[index+1]
		if frame >= left.Frame && frame < right.Frame {
			weight := float64(frame-left.Frame) / float64(right.Frame-left.Frame)
			return left.GainDB + (right.GainDB-left.GainDB)*weight
		}
	}
	return points[last].GainDB
}
