package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/repro"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

// The drill is a rehearsal, not a service path. It builds a mixed-rate stereo
// project through the real HTTP surface, captures it as a bundle, and then
// proves which layer a divergence comes from under each substituted input.
func main() {
	outDir := flag.String("out", "./var/repro-drill", "directory that receives rehearsal artifacts")
	flag.Parse()
	if err := run(*outDir); err != nil {
		fmt.Fprintf(os.Stderr, "repro drill failed: %v\n", err)
		os.Exit(1)
	}
}

type drillLine struct {
	Name   string         `json:"name"`
	Passed bool           `json:"passed"`
	Detail string         `json:"detail"`
	Report map[string]any `json:"report,omitempty"`
}

func run(outDir string) error {
	ctx := context.Background()
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return err
	}
	history := filepath.Join(outDir, "history")
	derived := filepath.Join(outDir, "derived")
	_ = os.RemoveAll(history)
	_ = os.RemoveAll(derived)
	if err := os.MkdirAll(history, 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(derived, 0o750); err != nil {
		return err
	}

	repo, err := storage.NewRepository(filepath.Join(outDir, "service-data"))
	if err != nil {
		return err
	}
	cfg := service.DefaultConfig()
	svc := service.New(repo, cfg)
	project, err := buildStudioProjectHTTP(ctx, repo, svc)
	if err != nil {
		return err
	}

	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	toolchain := repro.CurrentToolchain("voice-timeline-renderer", "", "drill-baseline", false)
	provenance := repro.ProvenanceForRender(toolchain, repro.ServiceConfig{
		MaxSourceBytes: cfg.MaxSourceBytes, MaxSpans: cfg.MaxSpans, MaxOutputFrames: cfg.MaxOutputFrames,
	}, "drill-producer", now)

	producerTrace := &renderTrace{}
	producerResult, err := renderWithTrace(ctx, project.plan, project.sources, cfg.MaxOutputFrames, now, producerTrace)
	if err != nil {
		return err
	}
	if digestOfBytes(producerResult.WAV) != project.outputDigest {
		return fmt.Errorf("producer trace replay diverged from the live render: %s vs %s",
			digestOfBytes(producerResult.WAV), project.outputDigest)
	}

	historicalDir := filepath.Join(history, "render-01")
	manifest, err := repro.BuildLiveBundle(ctx, repo, project.renderID, historicalDir, provenance, producerTrace.trace(), now)
	if err != nil {
		return err
	}

	lines := make([]drillLine, 0, 10)
	add := func(name string, passed bool, detail string, report any) {
		line := drillLine{Name: name, Passed: passed, Detail: detail}
		if report != nil {
			encoded, _ := json.Marshal(report)
			var decoded map[string]any
			_ = json.Unmarshal(encoded, &decoded)
			line.Report = decoded
		}
		lines = append(lines, line)
	}

	// 1. Baseline replay: exact same inputs and toolchain reproduce bytes.
	same, err := repro.Verify(ctx, historicalDir, repro.VerifyOptions{CapturedBy: "drill-same", Now: now})
	if err != nil {
		return err
	}
	add("same-toolchain-same-inputs", same.Verdict == repro.VerdictByteIdentical,
		fmt.Sprintf("verdict=%s earliest=%q", same.Verdict, same.EarliestDivergence), same)

	// 2. A re-render writes derived artifacts beside the history; the bundle
	//    must remain byte-for-byte unchanged and only volatile receipt fields
	//    may move.
	recreatedDir := filepath.Join(derived, "recreated-01")
	repeat, err := repro.Verify(ctx, historicalDir, repro.VerifyOptions{
		CapturedBy: "drill-repeat", Now: now.Add(time.Hour),
		WriteRecreated: true, RecreatedDir: recreatedDir,
	})
	if err != nil {
		return err
	}
	untouched, err := repro.ProveNoBundleMutation(historicalDir, manifest.ContentDigest)
	if err != nil {
		return err
	}
	receiptUntouched := bundleReceiptUntouched(historicalDir, manifest)
	add("re-created-artifacts-preserve-history",
		untouched && receiptUntouched && repeat.Verdict == repro.VerdictByteIdentical && repeat.ReceiptDifferencesOnlyVolatile(),
		fmt.Sprintf("bundle_unchanged=%v receipt_unchanged=%v verdict=%s volatile_only=%v",
			untouched, receiptUntouched, repeat.Verdict, repeat.ReceiptDifferencesOnlyVolatile()), repeat)

	// 3. The smallest honest hand-off when bytes cannot be shared: digests and
	//    receipts only. It must never claim reproduction.
	attestedDir := filepath.Join(history, "render-01-attested")
	if err := buildAttestedBundle(attestedDir, project, manifest, now); err != nil {
		return err
	}
	attested, err := repro.Verify(ctx, attestedDir, repro.VerifyOptions{CapturedBy: "drill-attested", Now: now})
	if err != nil {
		return err
	}
	add("attested-bundle-cannot-be-reproduced",
		attested.Verdict == repro.VerdictAttestedOnly && attested.EvidenceLevel == repro.LevelAttested,
		fmt.Sprintf("verdict=%s level=%s", attested.Verdict, attested.EvidenceLevel), attested)

	// 4. Source byte drift. Same timeline, same toolchain, three LSBs of
	//    additive offset on one source. Bytes must differ at source-bytes and
	//    the verdict must be signal-divergent.
	driftedWAV, err := repro.ShiftSamples(project.sourceAWAV, 3)
	if err != nil {
		return err
	}
	substitution, err := repro.Verify(ctx, historicalDir, repro.VerifyOptions{
		CapturedBy: "drill-source-drift", Now: now,
		Inputs: repro.ReplayInputs{
			Label:       "source-a-shifted-3-lsb",
			SourceMedia: map[string][]byte{project.sourceAID: driftedWAV},
		},
	})
	if err != nil {
		return err
	}
	add("source-byte-drift-locates-at-source-bytes",
		substitution.EarliestDivergence == repro.LayerSourceBytes && substitution.Verdict == repro.VerdictSignalDivergent,
		fmt.Sprintf("verdict=%s earliest=%q", substitution.Verdict, substitution.EarliestDivergence), substitution)

	// 5. Gain drift. Same source bytes and toolchain; span 0 gain changes by
	//    0.5 dB. Source, decode, resample, and map must all match, so the first
	//    differing layer is gain.
	gainPlan := project.plan
	gainPlan.Spans = append([]domain.TimelineSpan(nil), project.plan.Spans...)
	gainPlan.Spans[0].GainDB += 0.5
	gainShift, err := repro.Verify(ctx, historicalDir, repro.VerifyOptions{
		CapturedBy: "drill-gain-drift", Now: now,
		Inputs: repro.ReplayInputs{Label: "span-0-gain-plus-0.5db", Timeline: mustMarshal(gainPlan)},
	})
	if err != nil {
		return err
	}
	add("gain-drift-locates-at-gain",
		gainShift.EarliestDivergence == repro.LayerGain && gainShift.Verdict == repro.VerdictSignalDivergent,
		fmt.Sprintf("verdict=%s earliest=%q", gainShift.Verdict, gainShift.EarliestDivergence), gainShift)

	// 6. Resampler fault. The historical bundle is recorded by a pipeline whose
	//    nearest-neighbour resampler differs from the current linear one, while
	//    the declared toolchain and inputs stay identical. The first differing
	//    layer must be resample.
	foreignResult, err := repro.RenderForeign(project.renderID, project.plan, project.sources, cfg.MaxOutputFrames, now)
	if err != nil {
		return err
	}
	faultDir := filepath.Join(history, "render-01-resampler-fault")
	if _, err := repro.BuildBundle(faultDir, repro.BundleOptions{
		Project:     "voice-timeline-renderer",
		Timeline:    project.plan,
		TimelineRaw: project.timelineRaw,
		Sources:     project.bundledSources,
		Receipt:     foreignResult.Receipt,
		ReceiptRaw:  mustMarshal(foreignResult.Receipt),
		OutputWAV:   foreignResult.WAV,
		Trace: &repro.TraceEnvelope{
			Origin: "producer", Toolchain: toolchain, Trace: &foreignResult.Trace,
		},
		Provenance: repro.DeriveProvenance(toolchain, "drill-faulted-producer", manifest.ContentDigest, "faulted-trace", now),
		Now:        now,
	}); err != nil {
		return err
	}
	fault, err := repro.Verify(ctx, faultDir, repro.VerifyOptions{CapturedBy: "drill-fault", Toolchain: toolchain, Now: now})
	if err != nil {
		return err
	}
	add("resampler-fault-locates-at-resample",
		fault.EarliestDivergence == repro.LayerResample && fault.Verdict == repro.VerdictSignalDivergent,
		fmt.Sprintf("verdict=%s earliest=%q", fault.Verdict, fault.EarliestDivergence), fault)

	// 7. Foreign toolchain, honestly declared. Its bundle is self-consistent,
	//    but verifying it with the baseline contract must not produce a byte
	//    conclusion: the two runs are not byte-comparable.
	foreignToolchain := repro.VariantToolchain()
	foreignDir := filepath.Join(history, "render-01-foreign")
	if _, err := repro.BuildBundle(foreignDir, repro.BundleOptions{
		Project:     "voice-timeline-renderer",
		Timeline:    project.plan,
		TimelineRaw: project.timelineRaw,
		Sources:     project.bundledSources,
		Receipt:     foreignResult.Receipt,
		ReceiptRaw:  mustMarshal(foreignResult.Receipt),
		OutputWAV:   foreignResult.WAV,
		Trace: &repro.TraceEnvelope{
			Origin: "producer", Toolchain: foreignToolchain, Trace: &foreignResult.Trace,
		},
		Provenance: repro.DeriveProvenance(foreignToolchain, "drill-foreign", manifest.ContentDigest, "foreign-toolchain", now),
		Now:        now,
	}); err != nil {
		return err
	}
	selfForeign, err := repro.AuditBundle(ctx, foreignDir)
	if err != nil {
		return err
	}
	add("foreign-bundle-is-self-consistent", selfForeign.Passed,
		fmt.Sprintf("audit_passed=%v checks=%d level=%s", selfForeign.Passed, len(selfForeign.Checks), selfForeign.EvidenceLevel), selfForeign)

	crossVerdict, err := repro.Verify(ctx, foreignDir, repro.VerifyOptions{CapturedBy: "drill-cross", Toolchain: toolchain, Now: now})
	if err != nil {
		return err
	}
	add("foreign-toolchain-is-not-byte-comparable",
		crossVerdict.Verdict == repro.VerdictIncomparable && !crossVerdict.ByteComparable && !crossVerdict.AlgorithmCompatible,
		fmt.Sprintf("verdict=%s compatible=%v earliest=%q",
			crossVerdict.Verdict, crossVerdict.AlgorithmCompatible, crossVerdict.EarliestDivergence), crossVerdict)

	// 7b. Trace-free evidence. Same inputs, same output bytes as history, but
	//     the producer shipped no stage trace. A verifier must be unable to
	//     name a layer when a replay diverges.
	traceFreeDir := filepath.Join(history, "render-01-trace-free")
	traceFreeProvenance := repro.DeriveProvenance(toolchain, "drill-trace-free", manifest.ContentDigest, "trace-withheld", now)
	if _, err := repro.BuildTraceFreeCopy(traceFreeDir, historicalDir, manifest, traceFreeProvenance, now); err != nil {
		return err
	}
	faultProvenance := traceFreeProvenance
	_ = faultProvenance
	traceFree, err := repro.ExpectReplayWithoutTrace(ctx, traceFreeDir, repro.VerifyOptions{
		CapturedBy: "drill-trace-free", Now: now,
		Inputs: repro.ReplayInputs{
			Label:       "gain-plus-0.5db-no-trace",
			Timeline:    mustMarshal(gainPlan),
			SourceMedia: map[string][]byte{},
		},
	})
	if err != nil {
		return err
	}
	add("trace-free-evidence-cannot-name-a-layer",
		traceFree.Verdict == repro.VerdictSignalDivergent && traceFree.EarliestDivergence == "",
		fmt.Sprintf("verdict=%s earliest=%q", traceFree.Verdict, traceFree.EarliestDivergence), traceFree)

	// 8. The anti-pattern the convention forbids: rescale a different signal so
	//    its peak lands on the same level. Equal peak must not be called
	//    equivalent.
	peakTwin, err := repro.PeakShapedTwin(project.outputWAV)
	if err != nil {
		return err
	}
	direct, err := repro.InspectPair(project.outputWAV, peakTwin)
	if err != nil {
		return err
	}
	add("equal-peak-is-not-equivalence",
		direct.PeakDifferenceDB <= 0.05 && !direct.SignalEqual,
		fmt.Sprintf("peak_delta=%.4f dB rms_delta=%.4f dB max_sample_delta=%.5f signal_equal=%v",
			direct.PeakDifferenceDB, direct.RMSDifferenceDB, direct.MaxAbsoluteSampleDiff, direct.SignalEqual), direct)

	if err := writeDrillReport(outDir, project, historicalDir, lines); err != nil {
		return err
	}
	failures := 0
	for _, line := range lines {
		status := "PASS"
		if !line.Passed {
			status = "FAIL"
			failures++
		}
		fmt.Printf("%s  %-38s %s\n", status, line.Name, line.Detail)
	}
	if failures > 0 {
		return fmt.Errorf("%d drill assertion(s) failed; see %s", failures, filepath.Join(outDir, "drill.json"))
	}
	fmt.Printf("drill report: %s\n", filepath.Join(outDir, "drill.json"))
	return nil
}

func writeDrillReport(outDir string, project *studioProject, historyDir string, lines []drillLine) error {
	encoded, err := json.MarshalIndent(map[string]any{
		"schema_version": 1,
		"project":        describeProject(project),
		"history_bundle": historyDir,
		"results":        lines,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "drill.json"), append(encoded, '\n'), 0o640)
}

func describeProject(project *studioProject) map[string]any {
	return map[string]any{
		"render_id":        project.renderID,
		"timeline_id":      project.plan.ID,
		"output_digest":    project.outputDigest,
		"output_frames":    project.receipt.Output.FrameCount,
		"output_channels":  project.receipt.Output.Channels,
		"output_peak_dbfs": project.receipt.Output.PeakDBFS,
		"source_a":         fmt.Sprintf("channels=%d rate=%d frames=%d", project.sourceA.Channels, project.sourceA.SampleRate, project.sourceA.FrameCount),
		"source_b":         fmt.Sprintf("channels=%d rate=%d frames=%d", project.sourceB.Channels, project.sourceB.SampleRate, project.sourceB.FrameCount),
	}
}

func digestOfBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func mustMarshal(value any) []byte {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(encoded, '\n')
}
