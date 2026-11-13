package repro_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
	"voice-timeline-renderer/internal/repro"
)

func digestOf(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sineWAV(t *testing.T, rate, channels, frames int, frequency, amplitude float64) []byte {
	t.Helper()
	samples := make([]float32, frames*channels)
	for frame := 0; frame < frames; frame++ {
		value := float32(amplitude * float64(int(frame%64)-32) / 32)
		_ = frequency
		for channel := 0; channel < channels; channel++ {
			samples[frame*channels+channel] = value
		}
	}
	encoded, err := audiocodec.EncodePCM16(rate, channels, samples)
	if err != nil {
		t.Fatalf("encode source: %v", err)
	}
	return encoded
}

func fixture(t *testing.T) (*repro.Manifest, string, domain.Timeline, map[string]repro.BundledSource) {
	t.Helper()
	a := sineWAV(t, 44100, 1, 1200, 220, 0.3)
	b := sineWAV(t, 48000, 2, 900, 330, 0.2)
	assets := map[string]domain.SourceAsset{
		"source_a": {ID: "source_a", Name: "a", Digest: digestOf(t, a), ByteCount: int64(len(a)), SampleRate: 44100, Channels: 1, BitsPerSample: 16, FrameCount: 1200},
		"source_b": {ID: "source_b", Name: "b", Digest: digestOf(t, b), ByteCount: int64(len(b)), SampleRate: 48000, Channels: 2, BitsPerSample: 16, FrameCount: 900},
	}
	sources := map[string]repro.BundledSource{
		"source_a": {Asset: assets["source_a"], WAV: a},
		"source_b": {Asset: assets["source_b"], WAV: b},
	}
	plan := domain.Timeline{
		ID: "timeline_01", Name: "fixture", SampleRate: 48000, OutputChannels: 2,
		TargetPeakDBFS: -1,
		Spans: []domain.TimelineSpan{
			{SourceID: "source_a", SourceStartFrame: 0, SourceEndFrame: 1200, GainDB: -3},
			{SourceID: "source_b", SourceStartFrame: 0, SourceEndFrame: 900, GainDB: 0, CrossfadeFrames: 240},
		},
	}
	timelineRaw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatalf("marshal timeline: %v", err)
	}
	inputs := map[string]render.SourceInput{
		"source_a": {Asset: assets["source_a"], WAV: a},
		"source_b": {Asset: assets["source_b"], WAV: b},
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	trace := &render.Trace{}
	result, err := render.RenderWithTrace(context.Background(), "render_01", plan, inputs, render.Limits{MaxOutputFrames: 48000 * 180}, now, trace)
	if err != nil {
		t.Fatalf("render fixture: %v", err)
	}
	receiptRaw, err := json.MarshalIndent(result.Receipt, "", "  ")
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	receiptRaw = append(receiptRaw, '\n')
	dir := filepath.Join(t.TempDir(), "bundle")
	toolchain := repro.CurrentToolchain("voice-timeline-renderer", "", "test", false)
	manifest, err := repro.BuildBundle(dir, repro.BundleOptions{
		Project: "voice-timeline-renderer", Timeline: plan, TimelineRaw: timelineRaw,
		Sources: sources, Receipt: result.Receipt, ReceiptRaw: receiptRaw, OutputWAV: result.WAV,
		Trace:      &repro.TraceEnvelope{Origin: "producer", Toolchain: toolchain, Trace: trace},
		Provenance: repro.ProvenanceForRender(toolchain, repro.ServiceConfig{MaxOutputFrames: 48000 * 180}, "test", now),
		Now:        now,
	})
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	return &manifest, dir, plan, sources
}

func TestReplayIsByteIdentical(t *testing.T) {
	_, dir, _, _ := fixture(t)
	report, err := repro.Verify(context.Background(), dir, repro.VerifyOptions{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.Verdict != repro.VerdictByteIdentical {
		t.Fatalf("verdict = %s, want byte-identical", report.Verdict)
	}
	if report.EarliestDivergence != "" {
		t.Fatalf("earliest divergence = %q, want none", report.EarliestDivergence)
	}
	if !report.ReceiptDifferencesOnlyVolatile() {
		t.Fatalf("receipt differences beyond volatile fields: %+v", report.ReceiptDifferences)
	}
}

func TestVerifyDoesNotMutateBundle(t *testing.T) {
	manifest, dir, _, _ := fixture(t)
	before := manifest.ContentDigest
	if _, err := repro.Verify(context.Background(), dir, repro.VerifyOptions{}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	unchanged, err := repro.ProveNoBundleMutation(dir, before)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !unchanged {
		t.Fatal("verification mutated the historical bundle")
	}
}

func TestSignalTolerance(t *testing.T) {
	_, dir, _, _ := fixture(t)
	tolerance := repro.SignalTolerance{MaxAbsoluteSampleDiff: 4.0 / 32768, MaxRMSDifferenceDB: 0.5}
	report, err := repro.Verify(context.Background(), dir, repro.VerifyOptions{SignalTolerance: &tolerance})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.Verdict != repro.VerdictByteIdentical {
		t.Fatalf("verdict = %s", report.Verdict)
	}
}

func TestEqualPeakIsNotEquivalence(t *testing.T) {
	_, _, _, _ = fixture(t)
	base := sineWAV(t, 48000, 2, 800, 440, 0.5)
	twin, err := repro.PeakShapedTwin(base)
	if err != nil {
		t.Fatalf("peak twin: %v", err)
	}
	comparison, err := repro.InspectPair(base, twin)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if comparison.MaxAbsoluteSampleDiff == 0 {
		t.Fatalf("expected the waveform to change, got an identical sample stream")
	}
	if comparison.SignalEqual {
		t.Fatalf("peak-preserving waveform change was mistakenly called equal")
	}
}

func TestAttestedBundleCannotBeReproduced(t *testing.T) {
	manifest, dir, _, _ := fixture(t)
	attestedDir := filepath.Join(t.TempDir(), "attested")
	timelineRaw, _, err := repro.LoadHistoricalReceipt(dir, *manifest)
	if err != nil {
		t.Fatalf("load receipt: %v", err)
	}
	timeline, err := repro.LoadHistoricalTimeline(dir, *manifest)
	if err != nil {
		t.Fatalf("load timeline: %v", err)
	}
	if _, err := repro.BuildAttestedBundle(attestedDir, *manifest, timeline, timelineRaw,
		repro.Provenance{Producer: "test", SourceOfTruth: "attested-claims-only"}, time.Now()); err != nil {
		t.Fatalf("build attested: %v", err)
	}
	report, err := repro.Verify(context.Background(), attestedDir, repro.VerifyOptions{})
	if err != nil {
		t.Fatalf("verify attested: %v", err)
	}
	if report.Verdict != repro.VerdictAttestedOnly {
		t.Fatalf("verdict = %s, want attested-only-not-reproduced", report.Verdict)
	}
}

func TestRequiresLabelledSubstitution(t *testing.T) {
	_, dir, _, _ := fixture(t)
	_, err := repro.Verify(context.Background(), dir, repro.VerifyOptions{
		Inputs: repro.ReplayInputs{Timeline: []byte("{}")},
	})
	if err == nil {
		t.Fatal("substituted inputs without a label must be rejected")
	}
}

func TestCorruptBundleRejected(t *testing.T) {
	_, dir, _, _ := fixture(t)
	path := filepath.Join(dir, "output.wav")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if err := os.WriteFile(path, append(payload, 0), 0o640); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := repro.LoadBundle(dir); err == nil {
		t.Fatal("a tampered bundle must fail integrity checks")
	}
	_ = errors.New
}
