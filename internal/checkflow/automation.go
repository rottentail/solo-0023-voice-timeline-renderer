package checkflow

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
)

// runGainAutomation exercises the full persistence and render path of span
// gain automation curves, including resampling, cropping, crossfading,
// receipt facts, idempotency, and the strict rejection of malformed curves.
func runGainAutomation(ctx context.Context, harness *Harness) error {
	first, err := sineWAV(48000, 1, 400, 220, 0.25)
	if err != nil {
		return err
	}
	second, err := sineWAV(44100, 2, 350, 330, 0.2)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_auto_a", "opening take", first); err != nil || status != 201 {
		return fmt.Errorf("opening source setup failed: status=%d err=%v", status, err)
	}
	if _, status, err := harness.createSource(ctx, "source_auto_b", "closing take", second); err != nil || status != 201 {
		return fmt.Errorf("closing source setup failed: status=%d err=%v", status, err)
	}

	// First span output is 19200 frames; second is resampled 44100->48000 and
	// begins with a 4800-frame equal-power crossfade. The curve addresses output
	// frames relative to each span, so its timing survives resampling and the
	// crossfade overlap.
	spans := []domain.TimelineSpan{
		{
			SourceID: "source_auto_a", SourceStartFrame: 0, SourceEndFrame: 19200,
			GainDB: 0, CrossfadeFrames: 0,
			GainAutomation: &domain.GainAutomation{
				Interpolation: domain.GainInterpolation,
				Points: []domain.GainPoint{
					{Frame: 0, GainDB: -18},
					{Frame: 9600, GainDB: -9},
					{Frame: 19200, GainDB: 0},
				},
			},
		},
		{
			SourceID: "source_auto_b", SourceStartFrame: 0, SourceEndFrame: 15435,
			GainDB: 0, CrossfadeFrames: 4800,
			GainAutomation: &domain.GainAutomation{
				Interpolation: domain.GainInterpolation,
				Points: []domain.GainPoint{
					{Frame: 0, GainDB: -12},
					{Frame: 16800, GainDB: 0},
				},
			},
		},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_auto_01", "automated narration", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 || plan.State != "ready" {
		return fmt.Errorf("automated timeline write returned status=%d state=%s", status, plan.State)
	}
	readBack, status, err := harness.getTimeline(ctx, plan.ID)
	if err != nil {
		return err
	}
	if status != 200 || len(readBack.Spans) != 2 || readBack.Spans[0].GainAutomation == nil {
		return fmt.Errorf("automation curve was not persisted: status=%d", status)
	}
	persisted := readBack.Spans[0].GainAutomation
	if persisted.Interpolation != domain.GainInterpolation || len(persisted.Points) != 3 ||
		persisted.Points[1].Frame != 9600 || math.Abs(persisted.Points[1].GainDB-(-9)) > 1e-9 {
		return fmt.Errorf("persisted curve mismatch: %+v", persisted)
	}

	// Repeating the exact same request is idempotent and must not conflict.
	again, status, err := harness.createTimeline(ctx, "timeline_auto_01", "automated narration", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 200 || again.ID != plan.ID {
		return fmt.Errorf("idempotent timeline write returned status=%d", status)
	}

	job, status, err := harness.createRender(ctx, "render_auto_01", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || job.State != domain.RenderCompleted {
		return fmt.Errorf("automated render returned status=%d state=%s", status, job.State)
	}
	receipt, status, err := harness.getReceipt(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("receipt returned status=%d", status)
	}
	if len(receipt.Spans) != 2 || receipt.Spans[0].GainAutomation == nil || receipt.Spans[1].GainAutomation == nil {
		return fmt.Errorf("receipt is missing automation facts")
	}
	firstReceipt := receipt.Spans[0].GainAutomation
	if firstReceipt.Interpolation != domain.GainInterpolation || len(firstReceipt.Points) != 3 {
		return fmt.Errorf("receipt automation shape mismatch: %+v", firstReceipt)
	}
	if math.Abs(firstReceipt.EffectiveMinGainDB-(-18)) > 1e-9 || math.Abs(firstReceipt.EffectiveMaxGainDB-0) > 1e-9 {
		return fmt.Errorf("receipt effective extrema mismatch: min=%f max=%f", firstReceipt.EffectiveMinGainDB, firstReceipt.EffectiveMaxGainDB)
	}
	if math.Abs(firstReceipt.Points[1].EffectiveDB-(-9)) > 1e-9 {
		return fmt.Errorf("receipt effective breakpoint mismatch: %+v", firstReceipt.Points[1])
	}
	// Static gain must combine with automation gain additively in decibels.
	if math.Abs(receipt.Spans[1].GainAutomation.Points[0].EffectiveDB-(-12)) > 1e-9 {
		return fmt.Errorf("effective gain does not match static plus automation decibels")
	}

	output, status, err := harness.getOutput(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("output returned status=%d", status)
	}
	clip, _, err := audiocodec.DecodeWAV(output)
	if err != nil {
		return fmt.Errorf("decode automated output: %w", err)
	}
	// Verify the measured amplitude ramp matches the dB curve's expected linear
	// envelope. Normalization is a constant gain across both windows and
	// therefore cancels in the ratio; the source signal is the same sine.
	quiet := sectionRMS(clip, 200, 900)
	loud := sectionRMS(clip, 13500, 14300)
	if !(quiet > 0) || quiet >= loud {
		return fmt.Errorf("ramp not audible in output: quiet rms=%v loud rms=%v", quiet, loud)
	}
	expectedRatio := meanEnvelope(spans[0].GainAutomation, 13500, 14300) /
		meanEnvelope(spans[0].GainAutomation, 200, 900)
	observedRatio := loud / quiet
	if math.Abs(observedRatio-expectedRatio)/expectedRatio > 0.12 {
		return fmt.Errorf("automation ramp does not match curve: observed=%.3f expected=%.3f", observedRatio, expectedRatio)
	}

	// A second render of the same deterministic plan must produce the same bytes.
	jobTwo, status, err := harness.createRender(ctx, "render_auto_02", plan.ID)
	if err != nil || status != 201 || jobTwo.State != domain.RenderCompleted {
		return fmt.Errorf("second automated render failed: status=%d err=%v", status, err)
	}
	outputTwo, status, err := harness.getOutput(ctx, jobTwo.ID)
	if err != nil {
		return err
	}
	if len(outputTwo) != len(output) {
		return fmt.Errorf("deterministic render length changed: %d != %d", len(outputTwo), len(output))
	}
	for index := range output {
		if outputTwo[index] != output[index] {
			return fmt.Errorf("deterministic render bytes differ at offset %d", index)
		}
	}

	// Legacy timelines without a curve must not expose or persist the field.
	legacySpans := []domain.TimelineSpan{
		{SourceID: "source_auto_a", SourceStartFrame: 0, SourceEndFrame: 9600, GainDB: -3, CrossfadeFrames: 0},
	}
	legacyPlan, status, err := harness.createTimeline(ctx, "timeline_auto_legacy", "fixed gain", 48000, 1, -1, legacySpans)
	if err != nil || status != 201 {
		return fmt.Errorf("legacy timeline failed: status=%d err=%v", status, err)
	}
	rawStatus, rawBody, err := harness.requestJSON(ctx, "GET", "/v1/timelines/"+legacyPlan.ID, nil)
	if err != nil {
		return err
	}
	if rawStatus != 200 {
		return fmt.Errorf("legacy timeline read returned status=%d", rawStatus)
	}
	var raw map[string]any
	if err := json.Unmarshal(rawBody, &raw); err != nil {
		return err
	}
	var rawSpan map[string]any
	if spansValue, ok := raw["spans"].([]any); ok && len(spansValue) == 1 {
		rawSpan, _ = spansValue[0].(map[string]any)
	}
	if _, present := rawSpan["gain_automation"]; present {
		return fmt.Errorf("legacy timeline persisted a gain_automation field")
	}
	legacyJob, status, err := harness.createRender(ctx, "render_auto_legacy", legacyPlan.ID)
	if err != nil || status != 201 || legacyJob.State != domain.RenderCompleted {
		return fmt.Errorf("legacy render failed: status=%d err=%v", status, err)
	}
	legacyReceipt, status, err := harness.getReceipt(ctx, legacyJob.ID)
	if err != nil {
		return err
	}
	if legacyReceipt.Spans[0].GainAutomation != nil {
		return fmt.Errorf("legacy receipt unexpectedly contains automation facts")
	}

	if err := rejectAutomationCurves(ctx, harness); err != nil {
		return err
	}
	return nil
}

// rejectAutomationCurves verifies that every malformed curve is rejected with
// a stable error code and is never persisted or clamped into a valid plan.
func rejectAutomationCurves(ctx context.Context, harness *Harness) error {
	const outputFrames = int64(19200)
	validPoints := []domain.GainPoint{{Frame: 0, GainDB: -18}, {Frame: outputFrames, GainDB: 0}}
	cases := []struct {
		name     string
		curve    *domain.GainAutomation
		wantCode string
	}{
		{
			name:     "unsupported interpolation",
			curve:    &domain.GainAutomation{Interpolation: "cubic", Points: validPoints},
			wantCode: "invalid_automation_interpolation",
		},
		{
			name:     "single point",
			curve:    &domain.GainAutomation{Interpolation: domain.GainInterpolation, Points: validPoints[:1]},
			wantCode: "invalid_automation_curve",
		},
		{
			name: "point beyond span",
			curve: &domain.GainAutomation{Interpolation: domain.GainInterpolation, Points: []domain.GainPoint{
				{Frame: 0, GainDB: -18}, {Frame: outputFrames + 1, GainDB: 0},
			}},
			wantCode: "invalid_automation_time_range",
		},
		{
			name: "non increasing frames",
			curve: &domain.GainAutomation{Interpolation: domain.GainInterpolation, Points: []domain.GainPoint{
				{Frame: 0, GainDB: -18}, {Frame: 9600, GainDB: -10}, {Frame: 9600, GainDB: 0},
			}},
			wantCode: "invalid_automation_time_range",
		},
		{
			name: "missing start endpoint",
			curve: &domain.GainAutomation{Interpolation: domain.GainInterpolation, Points: []domain.GainPoint{
				{Frame: 100, GainDB: -18}, {Frame: outputFrames, GainDB: 0},
			}},
			wantCode: "invalid_automation_endpoint",
		},
		{
			name: "missing end endpoint",
			curve: &domain.GainAutomation{Interpolation: domain.GainInterpolation, Points: []domain.GainPoint{
				{Frame: 0, GainDB: -18}, {Frame: outputFrames - 1, GainDB: 0},
			}},
			wantCode: "invalid_automation_endpoint",
		},
		{
			name: "gain above range",
			curve: &domain.GainAutomation{Interpolation: domain.GainInterpolation, Points: []domain.GainPoint{
				{Frame: 0, GainDB: -18}, {Frame: outputFrames, GainDB: 13},
			}},
			wantCode: "invalid_automation_gain",
		},
		{
			name: "combined gain below range",
			curve: &domain.GainAutomation{Interpolation: domain.GainInterpolation, Points: []domain.GainPoint{
				{Frame: 0, GainDB: -1}, {Frame: outputFrames, GainDB: 0},
			}},
			wantCode: "invalid_automation_gain",
		},
		{
			name:     "negative point frame",
			curve:    &domain.GainAutomation{Interpolation: domain.GainInterpolation, Points: []domain.GainPoint{{Frame: -1, GainDB: -18}, {Frame: outputFrames, GainDB: 0}}},
			wantCode: "invalid_automation_time_range",
		},
		{
			name:     "more than 256 points",
			curve:    &domain.GainAutomation{Interpolation: domain.GainInterpolation, Points: oversizedCurvePoints(outputFrames)},
			wantCode: "invalid_automation_curve",
		},
	}
	for index, tc := range cases {
		staticGain := float64(0)
		if tc.name == "combined gain below range" {
			staticGain = -36
		}
		spans := []domain.TimelineSpan{{
			SourceID: "source_auto_a", SourceStartFrame: 0, SourceEndFrame: 19200,
			GainDB: staticGain, CrossfadeFrames: 0, GainAutomation: tc.curve,
		}}
		timelineID := fmt.Sprintf("timeline_auto_bad_%d", index)
		_, body, reqErr := harness.requestJSON(ctx, "PUT", "/v1/timelines/"+timelineID, service.TimelineWriteRequest{
			Name: "bad curve " + tc.name, SampleRate: 48000, OutputChannels: 1,
			TargetPeakDBFS: -1, Spans: spans,
		})
		if reqErr != nil {
			return fmt.Errorf("%s: request failed: %w", tc.name, reqErr)
		}
		var envelope failureEnvelope
		if err := decodeBody(body, &envelope); err != nil {
			return fmt.Errorf("%s: decode failure: %w body=%s", tc.name, err, string(body))
		}
		if envelope.Error.Code != tc.wantCode {
			return fmt.Errorf("%s: got code %q, want %q", tc.name, envelope.Error.Code, tc.wantCode)
		}
		if _, status, _ := harness.getTimeline(ctx, timelineID); status != 404 {
			return fmt.Errorf("%s: rejected timeline became visible with status %d", tc.name, status)
		}
	}
	return nil
}

// sectionRMS measures RMS over the inclusive half-open frame interval of a
// decoded clip, averaging across channels.
func sectionRMS(clip *audiocodec.Clip, startFrame, endFrame int64) float64 {
	channels := clip.Channels
	sumSquares := 0.0
	samples := 0
	for frame := startFrame; frame < endFrame; frame++ {
		for channel := 0; channel < channels; channel++ {
			value := float64(clip.Samples[int(frame)*channels+channel])
			sumSquares += value * value
			samples++
		}
	}
	if samples == 0 {
		return 0
	}
	return math.Sqrt(sumSquares / float64(samples))
}

// meanEnvelope averages the linear amplitude envelope implied by a linear-in-dB
// automation curve over a half-open frame interval, mirroring the renderer's
// breakpoint interpolation.
func meanEnvelope(curve *domain.GainAutomation, startFrame, endFrame int64) float64 {
	total := 0.0
	count := 0
	for frame := startFrame; frame < endFrame; frame++ {
		total += math.Pow(10, automationCurveDB(curve, frame)/20)
		count++
	}
	if count == 0 {
		return 0
	}
	return total / float64(count)
}

func automationCurveDB(curve *domain.GainAutomation, frame int64) float64 {
	points := curve.Points
	if frame <= points[0].Frame {
		return points[0].GainDB
	}
	last := len(points) - 1
	if frame >= points[last].Frame {
		return points[last].GainDB
	}
	for index := 0; index < last; index++ {
		left, right := points[index], points[index+1]
		if frame >= left.Frame && frame < right.Frame {
			weight := float64(frame-left.Frame) / float64(right.Frame-left.Frame)
			return left.GainDB + (right.GainDB-left.GainDB)*weight
		}
	}
	return points[last].GainDB
}

// oversizedCurvePoints builds a strictly increasing, endpoint-covering curve
// with one breakpoint beyond the 256-point limit.
func oversizedCurvePoints(lastFrame int64) []domain.GainPoint {
	points := make([]domain.GainPoint, 0, domain.MaxGainPoints+2)
	points = append(points, domain.GainPoint{Frame: 0, GainDB: -18})
	for frame := int64(1); frame <= domain.MaxGainPoints; frame++ {
		points = append(points, domain.GainPoint{Frame: frame, GainDB: 0})
	}
	points = append(points, domain.GainPoint{Frame: lastFrame, GainDB: 0})
	return points
}
