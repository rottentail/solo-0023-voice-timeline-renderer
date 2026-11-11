package checkflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
)

func Run(ctx context.Context, workflow string) error {
	harness, err := NewHarness()
	if err != nil {
		return err
	}
	defer harness.Close()

	switch workflow {
	case "ingest-audio-source":
		return runIngest(ctx, harness)
	case "compose-timeline":
		return runCompose(ctx, harness)
	case "render-mixdown":
		return runRender(ctx, harness)
	default:
		return fmt.Errorf("unknown workflow %q", workflow)
	}
}

func runIngest(ctx context.Context, harness *Harness) error {
	wav, err := sineWAV(48000, 1, 250, 440, 0.4)
	if err != nil {
		return err
	}
	asset, status, err := harness.createSource(ctx, "source_ingest_01", "narration take", wav)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("source write returned status %d, want 201", status)
	}
	if asset.FrameCount != 12000 || asset.BitsPerSample != 16 {
		return fmt.Errorf("source metadata is inconsistent: %+v", asset)
	}
	readBack, status, err := harness.getSource(ctx, asset.ID)
	if err != nil {
		return err
	}
	if status != 200 || readBack.Digest != asset.Digest {
		return fmt.Errorf("source read-back mismatch: status=%d digest=%s", status, readBack.Digest)
	}
	badStatus, body, err := harness.requestJSON(ctx, "PUT", "/v1/sources/source_ingest_bad", service.SourceWriteRequest{
		Name:      "broken take",
		WAVBase64: base64.StdEncoding.EncodeToString([]byte("not a wave")),
	})
	if err != nil {
		return err
	}
	if badStatus != 422 || failureCode(body) != "invalid_wav" {
		return fmt.Errorf("malformed media returned status=%d code=%s", badStatus, failureCode(body))
	}
	missing, status, err := harness.getSource(ctx, "source_ingest_bad")
	if err != nil {
		return err
	}
	if status != 404 || missing.ID != "" {
		return fmt.Errorf("failed source became visible: status=%d id=%s", status, missing.ID)
	}

	// The waveform surface must describe the same immutable source facts at a
	// requested timeline rate, remain byte-for-byte stable across reads, and
	// keep out-of-range requests distinct from true silence.
	query := "buckets=128&sample_rate=24000"
	summary, status, body, err := harness.getWaveform(ctx, asset.ID, query)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("waveform read returned status %d", status)
	}
	if summary.BucketCount != 128 || len(summary.Buckets) != 128 ||
		summary.SourceFrameCount != asset.FrameCount ||
		summary.SourceSampleRate != 48000 || summary.SampleRate != 24000 ||
		summary.OutputFrames != 6000 {
		return fmt.Errorf("waveform metadata is inconsistent: %+v", summary)
	}
	if summary.Silent {
		return fmt.Errorf("non-silent source was reported as silent")
	}
	if math.Abs(summary.SourcePeakDBFS-asset.PeakDBFS) > 1e-9 ||
		math.Abs(summary.PeakDBFS-asset.PeakDBFS) > 1e-9 {
		return fmt.Errorf("waveform peak facts disagree with source metadata: summary=%.6f source=%.6f", summary.PeakDBFS, asset.PeakDBFS)
	}
	if summary.Min > -0.35 || summary.Max < 0.35 {
		return fmt.Errorf("waveform range [%.6f, %.6f] does not contain the generated amplitude", summary.Min, summary.Max)
	}
	_, status, repeated, err := harness.getWaveform(ctx, asset.ID, query)
	if err != nil {
		return err
	}
	if status != 200 || !bytes.Equal(body, repeated) {
		return fmt.Errorf("repeated waveform read is not stable: status=%d", status)
	}

	rangeSummary, status, _, err := harness.getWaveform(ctx, asset.ID, "start_frame=1000&end_frame=6000&buckets=5&sample_rate=48000")
	if err != nil {
		return err
	}
	if status != 200 || rangeSummary.StartFrame != 1000 || rangeSummary.EndFrame != 6000 ||
		rangeSummary.OutputFrames != 5000 || len(rangeSummary.Buckets) != 5 ||
		rangeSummary.Buckets[0].StartFrame != 1000 || rangeSummary.Buckets[4].EndFrame != 6000 {
		return fmt.Errorf("sub-range waveform does not match the requested bounds: %+v", rangeSummary)
	}

	_, status, errorBody, err := harness.getWaveform(ctx, asset.ID, "end_frame=12001")
	if err != nil {
		return err
	}
	if status != 422 || failureCode(errorBody) != "waveform_range_out_of_bounds" {
		return fmt.Errorf("out-of-range waveform returned status=%d code=%s", status, failureCode(errorBody))
	}

	silentWAV, err := sineWAV(48000, 1, 100, 440, 0)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_ingest_silence", "silent take", silentWAV); err != nil || status != 201 {
		return fmt.Errorf("silent source setup failed: status=%d err=%v", status, err)
	}
	silent, status, _, err := harness.getWaveform(ctx, "source_ingest_silence", "buckets=8")
	if err != nil {
		return err
	}
	if status != 200 || !silent.Silent || silent.Min != 0 || silent.Max != 0 || silent.PeakDBFS != -120 {
		return fmt.Errorf("true silence was not distinguished: status=%d summary=%+v", status, silent)
	}
	return nil
}

func runCompose(ctx context.Context, harness *Harness) error {
	first, err := sineWAV(48000, 1, 300, 220, 0.35)
	if err != nil {
		return err
	}
	second, err := sineWAV(44100, 2, 250, 330, 0.25)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_compose_a", "first span", first); err != nil || status != 201 {
		return fmt.Errorf("first source setup failed: status=%d err=%v", status, err)
	}
	if _, status, err := harness.createSource(ctx, "source_compose_b", "second span", second); err != nil || status != 201 {
		return fmt.Errorf("second source setup failed: status=%d err=%v", status, err)
	}
	validSpans := []domain.TimelineSpan{
		{SourceID: "source_compose_a", SourceStartFrame: 0, SourceEndFrame: 14400, GainDB: -3, CrossfadeFrames: 0},
		{SourceID: "source_compose_b", SourceStartFrame: 0, SourceEndFrame: 11025, GainDB: 1, CrossfadeFrames: 2400},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_compose_01", "two span assembly", 48000, 2, -1, validSpans)
	if err != nil {
		return err
	}
	if status != 201 || plan.State != "ready" {
		return fmt.Errorf("timeline write returned status=%d state=%s", status, plan.State)
	}
	readBack, status, err := harness.getTimeline(ctx, plan.ID)
	if err != nil {
		return err
	}
	if status != 200 || readBack.OutputFrames != plan.OutputFrames || len(readBack.Spans) != 2 {
		return fmt.Errorf("timeline read-back mismatch: status=%d frames=%d", status, readBack.OutputFrames)
	}
	invalidSpans := append([]domain.TimelineSpan(nil), validSpans...)
	invalidSpans[1].CrossfadeFrames = 999999
	_, status, err = harness.createTimeline(ctx, "timeline_compose_bad", "invalid crossfade", 48000, 2, -1, invalidSpans)
	if err != nil {
		return err
	}
	if status != 422 {
		return fmt.Errorf("invalid crossfade returned status %d, want 422", status)
	}
	return nil
}

func runRender(ctx context.Context, harness *Harness) error {
	first, err := sineWAV(48000, 1, 400, 220, 0.25)
	if err != nil {
		return err
	}
	second, err := sineWAV(44100, 2, 350, 330, 0.2)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_render_a", "opening take", first); err != nil || status != 201 {
		return fmt.Errorf("opening source setup failed: status=%d err=%v", status, err)
	}
	if _, status, err := harness.createSource(ctx, "source_render_b", "closing take", second); err != nil || status != 201 {
		return fmt.Errorf("closing source setup failed: status=%d err=%v", status, err)
	}
	spans := []domain.TimelineSpan{
		{SourceID: "source_render_a", SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: -2, CrossfadeFrames: 0},
		{SourceID: "source_render_b", SourceStartFrame: 0, SourceEndFrame: 15435, GainDB: 0, CrossfadeFrames: 4800},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_render_01", "crossfaded narration", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("timeline setup returned status %d", status)
	}
	job, status, err := harness.createRender(ctx, "render_mixdown_01", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || job.State != domain.RenderCompleted || job.Output == nil {
		return fmt.Errorf("render returned status=%d state=%s output=%v", status, job.State, job.Output)
	}
	fetched, status, err := harness.getRender(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 || fetched.State != domain.RenderCompleted {
		return fmt.Errorf("render read-back returned status=%d state=%s", status, fetched.State)
	}
	receipt, status, err := harness.getReceipt(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 || receipt.Output.FrameCount != plan.OutputFrames {
		return fmt.Errorf("receipt frame mismatch: status=%d receipt=%d plan=%d", status, receipt.Output.FrameCount, plan.OutputFrames)
	}
	audioBytes, status, err := harness.getOutput(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("output returned status %d", status)
	}
	digest := sha256.Sum256(audioBytes)
	if hex.EncodeToString(digest[:]) != receipt.Output.Digest {
		return fmt.Errorf("output digest does not match receipt")
	}
	clip, info, err := audiocodec.DecodeWAV(audioBytes)
	if err != nil {
		return fmt.Errorf("decode rendered output: %w", err)
	}
	measured := audiocodec.Measure(clip.Samples)
	if info.FrameCount != receipt.Output.FrameCount {
		return fmt.Errorf("decoded frame count %d != receipt %d", info.FrameCount, receipt.Output.FrameCount)
	}
	if math.Abs(measured.PeakDBFS-(-1)) > 0.25 {
		return fmt.Errorf("output peak %.3f dBFS does not meet -1 dBFS target", measured.PeakDBFS)
	}
	return nil
}
