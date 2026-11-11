package checkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
)

func sha256Hex(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// runChannelRouting exercises the per-span pan and channel extraction settings
// end to end. It proves that explicit routing produces the exact declared
// stereo coefficients, that repeated renders are byte-identical, that
// unconfigured spans keep the legacy mapping, and that illegal matrices, mono
// output, and duplicate renders never leave an ambiguous channel decision.
func runChannelRouting(ctx context.Context, harness *Harness) error {
	monoWAV, err := sineWAV(48000, 1, 200, 220, 0.5)
	if err != nil {
		return err
	}
	stereoWAV, err := sineWAV(48000, 2, 200, 330, 0.5)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_routing_mono", "mono narration", monoWAV); err != nil || status != 201 {
		return fmt.Errorf("mono source setup failed: status=%d err=%v", status, err)
	}
	if _, status, err := harness.createSource(ctx, "source_routing_stereo", "stereo material", stereoWAV); err != nil || status != 201 {
		return fmt.Errorf("stereo source setup failed: status=%d err=%v", status, err)
	}
	monoClip, _, err := audiocodec.DecodeWAV(monoWAV)
	if err != nil {
		return err
	}
	stereoClip, _, err := audiocodec.DecodeWAV(stereoWAV)
	if err != nil {
		return err
	}

	if err := verifyExplicitRouting(ctx, harness, monoClip, stereoClip); err != nil {
		return err
	}
	if err := verifyCenterPan(ctx, harness, monoClip); err != nil {
		return err
	}
	if err := verifySelectRight(ctx, harness, stereoClip); err != nil {
		return err
	}
	if err := verifyLegacyMapping(ctx, harness); err != nil {
		return err
	}
	if err := verifyInvalidRouting(ctx, harness); err != nil {
		return err
	}
	if err := verifyRoutingIdempotency(ctx, harness); err != nil {
		return err
	}
	if err := verifyRepeatableRenders(ctx, harness); err != nil {
		return err
	}
	return nil
}

func verifyRoutingIdempotency(ctx context.Context, harness *Harness) error {
	pan := 0.4
	spans := []domain.TimelineSpan{
		{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 2400, GainDB: 0, CrossfadeFrames: 0, Pan: &pan},
	}
	first, status, err := harness.createTimeline(ctx, "timeline_routing_idem", "routing idempotency", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("routing idempotency setup returned status %d", status)
	}
	again, status, err := harness.createTimeline(ctx, "timeline_routing_idem", "routing idempotency", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 200 || again.ID != first.ID || again.Spans[0].Pan == nil || *again.Spans[0].Pan != pan {
		return fmt.Errorf("identical routed timeline was not idempotent: status=%d", status)
	}
	changed := 0.5
	changedSpans := []domain.TimelineSpan{
		{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 2400, GainDB: 0, CrossfadeFrames: 0, Pan: &changed},
	}
	status, body, err := harness.requestJSON(ctx, "PUT", "/v1/timelines/timeline_routing_idem", service.TimelineWriteRequest{
		Name: "routing idempotency", SampleRate: 48000, OutputChannels: 2, TargetPeakDBFS: -1, Spans: changedSpans,
	})
	if err != nil {
		return err
	}
	if status != 409 || failureCode(body) != "timeline_conflict" {
		return fmt.Errorf("changed pan returned status=%d code=%s, want 409 timeline_conflict", status, failureCode(body))
	}
	return nil
}

func verifyExplicitRouting(ctx context.Context, harness *Harness, monoClip, stereoClip *audiocodec.Clip) error {
	hardRight := 1.0
	spans := []domain.TimelineSpan{
		{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 4800, GainDB: 0, CrossfadeFrames: 0, Pan: &hardRight},
		{SourceID: "source_routing_stereo", SourceStartFrame: 0, SourceEndFrame: 4800, GainDB: 0, CrossfadeFrames: 2400, ChannelSelect: domain.ChannelSelectLeft},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_routing_01", "routed assembly", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("routed timeline returned status %d", status)
	}

	first, status, err := harness.createRender(ctx, "render_routing_a", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || first.State != domain.RenderCompleted {
		return fmt.Errorf("first routed render returned status=%d state=%s", status, first.State)
	}
	second, status, err := harness.createRender(ctx, "render_routing_b", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || second.State != domain.RenderCompleted {
		return fmt.Errorf("second routed render returned status=%d state=%s", status, second.State)
	}
	firstWAV, _, err := harness.getOutput(ctx, first.ID)
	if err != nil {
		return err
	}
	secondWAV, _, err := harness.getOutput(ctx, second.ID)
	if err != nil {
		return err
	}
	if string(firstWAV) != string(secondWAV) {
		return fmt.Errorf("routed renders of the same timeline are not byte-identical")
	}

	receipt, status, err := harness.getReceipt(ctx, first.ID)
	if err != nil {
		return err
	}
	if status != 200 || len(receipt.Spans) != 2 {
		return fmt.Errorf("routed receipt mismatch: status=%d spans=%d", status, len(receipt.Spans))
	}
	panSpan := receipt.Spans[0]
	if panSpan.ChannelAction != audiocodec.ActionPan || panSpan.Pan == nil || *panSpan.Pan != 1.0 {
		return fmt.Errorf("pan span receipt action=%q pan=%v, want %s pan=1", panSpan.ChannelAction, panSpan.Pan, audiocodec.ActionPan)
	}
	if len(panSpan.ChannelMatrix) != 4 {
		return fmt.Errorf("hard-right pan matrix is missing coefficients: %v", panSpan.ChannelMatrix)
	}
	for index, want := range []float64{0, 0, 0, 1} {
		if math.Abs(panSpan.ChannelMatrix[index]-want) > 1e-12 {
			return fmt.Errorf("hard-right pan matrix is not effectively [0 0 0 1]: %v", panSpan.ChannelMatrix)
		}
	}
	selectSpan := receipt.Spans[1]
	if selectSpan.ChannelAction != audiocodec.ActionSelectLeft || selectSpan.ChannelSelect != "left" {
		return fmt.Errorf("select span receipt action=%q select=%q", selectSpan.ChannelAction, selectSpan.ChannelSelect)
	}
	if len(selectSpan.ChannelMatrix) != 4 || selectSpan.ChannelMatrix[0] != 1 || selectSpan.ChannelMatrix[1] != 0 || selectSpan.ChannelMatrix[2] != 0 || selectSpan.ChannelMatrix[3] != 0 {
		return fmt.Errorf("select-left matrix is not [1 0 0 0]: %v", selectSpan.ChannelMatrix)
	}
	if len(receipt.Warnings) != 2 {
		return fmt.Errorf("routed receipt should warn about both spans, got %v", receipt.Warnings)
	}

	output, info, err := audiocodec.DecodeWAV(firstWAV)
	if err != nil {
		return err
	}
	if info.Channels != 2 || info.FrameCount != plan.OutputFrames {
		return fmt.Errorf("routed output layout mismatch: channels=%d frames=%d", info.Channels, info.FrameCount)
	}
	// The first 2400 frames are exclusive to the hard-right panned mono span.
	if err := assertSilentChannel(output, 0, 2400, 0); err != nil {
		return fmt.Errorf("hard-right pan leaked into the left channel: %w", err)
	}
	if err := assertActiveChannel(output, 0, 2400, 1); err != nil {
		return fmt.Errorf("hard-right pan produced no right-channel signal: %w", err)
	}
	if err := assertChannelMatches(output, 0, 2400, 0, 1, monoClip, 0); err != nil {
		return fmt.Errorf("hard-right pan does not carry the mono source: %w", err)
	}
	// Frames after the crossfade are exclusive to the select-left stereo span.
	postFadeStart := selectSpan.OutputStartFrame + selectSpan.CrossfadeFrames
	postFadeEnd := selectSpan.OutputEndFrame
	if err := assertSilentChannel(output, postFadeStart, postFadeEnd, 1); err != nil {
		return fmt.Errorf("select-left leaked into the right channel: %w", err)
	}
	if err := assertChannelMatches(output, postFadeStart, postFadeEnd, selectSpan.OutputStartFrame, 0, stereoClip, 0); err != nil {
		return fmt.Errorf("select-left does not carry the stereo left channel: %w", err)
	}
	return nil
}

func verifyCenterPan(ctx context.Context, harness *Harness, monoClip *audiocodec.Clip) error {
	center := 0.0
	spans := []domain.TimelineSpan{
		{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 2400, GainDB: 0, CrossfadeFrames: 0, Pan: &center},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_routing_center", "center panned", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("center-pan timeline returned status %d", status)
	}
	job, status, err := harness.createRender(ctx, "render_routing_center", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || job.State != domain.RenderCompleted {
		return fmt.Errorf("center-pan render returned status=%d state=%s", status, job.State)
	}
	receipt, _, err := harness.getReceipt(ctx, job.ID)
	if err != nil {
		return err
	}
	expected := math.Cos(math.Pi / 4)
	matrix := receipt.Spans[0].ChannelMatrix
	if len(matrix) != 4 || math.Abs(matrix[0]-expected) > 1e-12 || math.Abs(matrix[3]-expected) > 1e-12 || matrix[1] != 0 || matrix[2] != 0 {
		return fmt.Errorf("center-pan matrix is not equal-power %.6f: %v", expected, matrix)
	}
	wav, _, err := harness.getOutput(ctx, job.ID)
	if err != nil {
		return err
	}
	output, _, err := audiocodec.DecodeWAV(wav)
	if err != nil {
		return err
	}
	for frame := int64(0); frame < 2400; frame++ {
		left := output.Samples[frame*2]
		right := output.Samples[frame*2+1]
		if left != right {
			return fmt.Errorf("center pan is not dual-mono at frame %d: L=%v R=%v", frame, left, right)
		}
	}
	return nil
}

func verifySelectRight(ctx context.Context, harness *Harness, stereoClip *audiocodec.Clip) error {
	spans := []domain.TimelineSpan{
		{SourceID: "source_routing_stereo", SourceStartFrame: 0, SourceEndFrame: 2400, GainDB: 0, CrossfadeFrames: 0, ChannelSelect: domain.ChannelSelectRight},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_routing_right", "right extraction", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("select-right timeline returned status %d", status)
	}
	job, status, err := harness.createRender(ctx, "render_routing_right", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || job.State != domain.RenderCompleted {
		return fmt.Errorf("select-right render returned status=%d state=%s", status, job.State)
	}
	receipt, _, err := harness.getReceipt(ctx, job.ID)
	if err != nil {
		return err
	}
	if receipt.Spans[0].ChannelAction != audiocodec.ActionSelectRight {
		return fmt.Errorf("select-right receipt action = %q", receipt.Spans[0].ChannelAction)
	}
	wav, _, err := harness.getOutput(ctx, job.ID)
	if err != nil {
		return err
	}
	output, _, err := audiocodec.DecodeWAV(wav)
	if err != nil {
		return err
	}
	if err := assertSilentChannel(output, 0, 2400, 0); err != nil {
		return fmt.Errorf("select-right leaked into the left channel: %w", err)
	}
	if err := assertChannelMatches(output, 0, 2400, 0, 1, stereoClip, 1); err != nil {
		return fmt.Errorf("select-right does not carry the stereo right channel: %w", err)
	}
	return nil
}

func verifyLegacyMapping(ctx context.Context, harness *Harness) error {
	spans := []domain.TimelineSpan{
		{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 2400, GainDB: -2, CrossfadeFrames: 0},
		{SourceID: "source_routing_stereo", SourceStartFrame: 0, SourceEndFrame: 2400, GainDB: 0, CrossfadeFrames: 0},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_routing_legacy", "unconfigured mapping", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("legacy timeline returned status %d", status)
	}
	first, status, err := harness.createRender(ctx, "render_legacy_a", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || first.State != domain.RenderCompleted {
		return fmt.Errorf("legacy render returned status=%d state=%s", status, first.State)
	}
	second, status, err := harness.createRender(ctx, "render_legacy_b", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("second legacy render returned status %d", status)
	}
	firstWAV, _, err := harness.getOutput(ctx, first.ID)
	if err != nil {
		return err
	}
	secondWAV, _, err := harness.getOutput(ctx, second.ID)
	if err != nil {
		return err
	}
	if string(firstWAV) != string(secondWAV) {
		return fmt.Errorf("legacy renders are not byte-identical")
	}
	receipt, _, err := harness.getReceipt(ctx, first.ID)
	if err != nil {
		return err
	}
	for _, span := range receipt.Spans {
		if span.Pan != nil || span.ChannelSelect != "" || len(span.ChannelMatrix) != 0 {
			return fmt.Errorf("legacy span %d leaked explicit routing fields: %+v", span.Index, span)
		}
		switch span.ChannelAction {
		case audiocodec.ActionUnchanged, audiocodec.ActionMonoToStereo, audiocodec.ActionStereoToMono:
		default:
			return fmt.Errorf("legacy span %d reports unknown action %q", span.Index, span.ChannelAction)
		}
	}
	if receipt.Spans[0].ChannelAction != audiocodec.ActionMonoToStereo || receipt.Spans[1].ChannelAction != audiocodec.ActionUnchanged {
		return fmt.Errorf("legacy actions changed: %q %q", receipt.Spans[0].ChannelAction, receipt.Spans[1].ChannelAction)
	}
	output, _, err := audiocodec.DecodeWAV(firstWAV)
	if err != nil {
		return err
	}
	active := false
	for frame := int64(0); frame < 2400; frame++ {
		if output.Samples[frame*2] != output.Samples[frame*2+1] {
			return fmt.Errorf("legacy mono span is not dual-mono at frame %d", frame)
		}
		if output.Samples[frame*2] != 0 {
			active = true
		}
	}
	if !active {
		return fmt.Errorf("legacy mono span produced no signal")
	}
	return nil
}

func verifyInvalidRouting(ctx context.Context, harness *Harness) error {
	offRange := 1.5
	center := 0.0
	cases := []struct {
		name       string
		timelineID string
		channels   int
		span       domain.TimelineSpan
		wantCode   string
	}{
		{
			name:       "unknown channel_select keyword",
			timelineID: "timeline_routing_bad_matrix",
			channels:   2,
			span:       domain.TimelineSpan{SourceID: "source_routing_stereo", SourceStartFrame: 0, SourceEndFrame: 2400, ChannelSelect: "middle"},
			wantCode:   "invalid_channel_matrix",
		},
		{
			name:       "pan out of range",
			timelineID: "timeline_routing_bad_pan",
			channels:   2,
			span:       domain.TimelineSpan{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 2400, Pan: &offRange},
			wantCode:   "invalid_pan",
		},
		{
			name:       "pan and channel_select together",
			timelineID: "timeline_routing_bad_combo",
			channels:   2,
			span:       domain.TimelineSpan{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 2400, Pan: &center, ChannelSelect: domain.ChannelSelectLeft},
			wantCode:   "invalid_channel_route",
		},
		{
			name:       "pan on a stereo source",
			timelineID: "timeline_routing_bad_pan_source",
			channels:   2,
			span:       domain.TimelineSpan{SourceID: "source_routing_stereo", SourceStartFrame: 0, SourceEndFrame: 2400, Pan: &center},
			wantCode:   "invalid_channel_route",
		},
		{
			name:       "channel_select on a mono source",
			timelineID: "timeline_routing_bad_select_source",
			channels:   2,
			span:       domain.TimelineSpan{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 2400, ChannelSelect: domain.ChannelSelectLeft},
			wantCode:   "invalid_channel_route",
		},
		{
			name:       "pan with mono output",
			timelineID: "timeline_routing_bad_mono_pan",
			channels:   1,
			span:       domain.TimelineSpan{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 2400, Pan: &center},
			wantCode:   "invalid_channel_route",
		},
		{
			name:       "channel_select with mono output",
			timelineID: "timeline_routing_bad_mono_select",
			channels:   1,
			span:       domain.TimelineSpan{SourceID: "source_routing_stereo", SourceStartFrame: 0, SourceEndFrame: 2400, ChannelSelect: domain.ChannelSelectLeft},
			wantCode:   "invalid_channel_route",
		},
	}
	for _, tc := range cases {
		request := service.TimelineWriteRequest{
			Name:           tc.name,
			SampleRate:     48000,
			OutputChannels: tc.channels,
			TargetPeakDBFS: -1,
			Spans:          []domain.TimelineSpan{tc.span},
		}
		status, body, err := harness.requestJSON(ctx, "PUT", "/v1/timelines/"+tc.timelineID, request)
		if err != nil {
			return err
		}
		if status != 422 || failureCode(body) != tc.wantCode {
			return fmt.Errorf("%s: status=%d code=%s, want 422 %s", tc.name, status, failureCode(body), tc.wantCode)
		}
		if _, readStatus, err := harness.getTimeline(ctx, tc.timelineID); err != nil {
			return err
		} else if readStatus != 404 {
			return fmt.Errorf("%s: rejected timeline became visible with status %d", tc.name, readStatus)
		}
	}
	return nil
}

func verifyRepeatableRenders(ctx context.Context, harness *Harness) error {
	center := -0.5
	spans := []domain.TimelineSpan{
		{SourceID: "source_routing_mono", SourceStartFrame: 0, SourceEndFrame: 4800, GainDB: 0, CrossfadeFrames: 0, Pan: &center},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_routing_repeat", "repeatable render", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("repeat-render timeline returned status %d", status)
	}
	first, status, err := harness.createRender(ctx, "render_routing_repeat", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || first.State != domain.RenderCompleted {
		return fmt.Errorf("repeat render first call returned status=%d state=%s", status, first.State)
	}
	duplicate, status, err := harness.createRender(ctx, "render_routing_repeat", plan.ID)
	if err != nil {
		return err
	}
	if status != 200 || duplicate.ID != first.ID || duplicate.State != domain.RenderCompleted {
		return fmt.Errorf("duplicate render call returned status=%d state=%s", status, duplicate.State)
	}
	if duplicate.Output == nil || first.Output == nil || duplicate.Output.Digest != first.Output.Digest {
		return fmt.Errorf("duplicate render changed the output digest")
	}
	wav, _, err := harness.getOutput(ctx, first.ID)
	if err != nil {
		return err
	}
	receipt, _, err := harness.getReceipt(ctx, first.ID)
	if err != nil {
		return err
	}
	if digest := sha256Hex(wav); digest != receipt.Output.Digest {
		return fmt.Errorf("repeated output digest %s does not match receipt %s", digest, receipt.Output.Digest)
	}
	return nil
}

func assertSilentChannel(clip *audiocodec.Clip, startFrame, endFrame int64, channel int) error {
	for frame := startFrame; frame < endFrame; frame++ {
		if clip.Samples[frame*2+int64(channel)] != 0 {
			return fmt.Errorf("frame %d channel %d is not silent", frame, channel)
		}
	}
	return nil
}

func assertActiveChannel(clip *audiocodec.Clip, startFrame, endFrame int64, channel int) error {
	active := false
	for frame := startFrame; frame < endFrame; frame++ {
		if clip.Samples[frame*2+int64(channel)] != 0 {
			active = true
		}
	}
	if !active {
		return fmt.Errorf("channel %d is silent across frames %d-%d", channel, startFrame, endFrame)
	}
	return nil
}

// assertChannelMatches verifies that an output region carries the referenced
// source channel under one uniform scalar (the normalization gain). The ratio
// is estimated from high-amplitude frames where int16 quantization is
// negligible, then every frame is checked against an absolute tolerance.
// spanOutputStart is the output frame where the span begins, so output frames
// map back to source frames with frame-spanOutputStart.
func assertChannelMatches(output *audiocodec.Clip, startFrame, endFrame, spanOutputStart int64, outputChannel int, source *audiocodec.Clip, sourceChannel int) error {
	const (
		anchorFloor = 0.1
		sampleTol   = 2.0 / 32767
		ratioTol    = 5e-4
	)
	var ratios []float64
	for frame := startFrame; frame < endFrame; frame++ {
		got := float64(output.Samples[frame*2+int64(outputChannel)])
		want := float64(source.Samples[(frame-spanOutputStart)*int64(source.Channels)+int64(sourceChannel)])
		if math.Abs(want) >= anchorFloor {
			ratios = append(ratios, got/want)
		}
	}
	if len(ratios) == 0 {
		return fmt.Errorf("output region has no high-amplitude frames to establish the gain ratio")
	}
	ratio := 0.0
	for _, value := range ratios {
		ratio += value
	}
	ratio /= float64(len(ratios))
	if ratio <= 0 {
		return fmt.Errorf("output/source ratio is not positive: %v", ratio)
	}
	for _, value := range ratios {
		if math.Abs(value-ratio) > ratioTol {
			return fmt.Errorf("output/source ratio is not uniform: %v vs %v", value, ratio)
		}
	}
	for frame := startFrame; frame < endFrame; frame++ {
		got := float64(output.Samples[frame*2+int64(outputChannel)])
		want := float64(source.Samples[(frame-spanOutputStart)*int64(source.Channels)+int64(sourceChannel)])
		if math.Abs(got-want*ratio) > sampleTol {
			return fmt.Errorf("frame %d output %.6f does not equal source %.6f x %.6f", frame, got, want, ratio)
		}
	}
	return nil
}
