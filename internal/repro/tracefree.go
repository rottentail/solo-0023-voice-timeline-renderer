package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"voice-timeline-renderer/internal/domain"
)

// BuildTraceFreeCopy writes a bundle with the same inputs and historical output
// but no producer stage trace. A verifier replaying it must still detect that
// the output differs, yet must refuse to name the layer responsible.
func BuildTraceFreeCopy(outDir string, historicalDir string, manifest Manifest, provenance Provenance, now time.Time) (Manifest, error) {
	if outDir == "" {
		return Manifest{}, fmt.Errorf("trace-free copy output directory is required")
	}
	timelineRaw, err := os.ReadFile(filepath.Join(historicalDir, filepath.FromSlash(manifest.TimelineFile)))
	if err != nil {
		return Manifest{}, err
	}
	receiptRaw, err := os.ReadFile(filepath.Join(historicalDir, filepath.FromSlash(manifest.ReceiptFile)))
	if err != nil {
		return Manifest{}, err
	}
	var outputBytes []byte
	if manifest.OutputFile != "" {
		outputBytes, err = os.ReadFile(filepath.Join(historicalDir, filepath.FromSlash(manifest.OutputFile)))
		if err != nil {
			return Manifest{}, err
		}
	}
	sources := make(map[string]BundledSource, len(manifest.Sources))
	for _, record := range manifest.Sources {
		if record.MediaFile == "" || record.MetaFile == "" {
			return Manifest{}, fmt.Errorf("bundle is attested; a trace-free replayable copy needs source bytes")
		}
		wav, err := os.ReadFile(filepath.Join(historicalDir, filepath.FromSlash(record.MediaFile)))
		if err != nil {
			return Manifest{}, err
		}
		metadata, err := os.ReadFile(filepath.Join(historicalDir, filepath.FromSlash(record.MetaFile)))
		if err != nil {
			return Manifest{}, err
		}
		sources[record.ID] = BundledSource{
			Asset:       assetFromRecord(record),
			MetadataRaw: metadata,
			WAV:         wav,
		}
	}
	options := BundleOptions{
		Project:     manifest.Project,
		TimelineRaw: timelineRaw,
		Sources:     sources,
		ReceiptRaw:  receiptRaw,
		OutputWAV:   outputBytes,
		Provenance:  provenance,
		Now:         now,
	}
	return BuildBundle(outDir, options)
}

func assetFromRecord(record SourceRecord) domain.SourceAsset {
	return domain.SourceAsset{
		ID: record.ID, Name: record.Name, Digest: record.Digest,
		ByteCount: record.ByteCount, SampleRate: record.SampleRate, Channels: record.Channels,
		BitsPerSample: record.BitsPerSample, FrameCount: record.FrameCount,
		DurationMS: record.DurationMS, PeakDBFS: record.PeakDBFS, RMSDBFS: record.RMSDBFS,
	}
}

// MarshalJSONForCLI is a small helper used by the command so reports stay
// stable across machines.
func MarshalJSONForCLI(value any) ([]byte, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// ExpectReplayWithoutTrace is a drill helper: it runs verification and asserts
// the report cannot name a layer.
func ExpectReplayWithoutTrace(ctx context.Context, bundleDir string, options VerifyOptions) (VerificationReport, error) {
	report, err := Verify(ctx, bundleDir, options)
	if err != nil {
		return VerificationReport{}, err
	}
	if report.Verdict == VerdictByteIdentical || report.Verdict == VerdictSampleIdentical {
		return report, fmt.Errorf("trace-free bundle unexpectedly reproduced the historical output (%s)", report.Verdict)
	}
	if report.EarliestDivergence != "" {
		return report, fmt.Errorf("trace-free bundle named a responsible layer %q without a historical trace", report.EarliestDivergence)
	}
	if report.Verdict != VerdictSignalDivergent {
		return report, fmt.Errorf("trace-free divergent replay produced verdict %s", report.Verdict)
	}
	return report, nil
}
