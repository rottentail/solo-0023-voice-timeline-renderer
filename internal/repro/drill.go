package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// InspectPair compares two WAV byte payloads directly with the same tolerance
// and the same anti-pattern check that Verify applies.
func InspectPair(historical, current []byte) (*OutputComparison, error) {
	return InspectPairWithTolerance(historical, current, DefaultSignalTolerance())
}

func InspectPairWithTolerance(historical, current []byte, tolerance SignalTolerance) (*OutputComparison, error) {
	historicalClip, historicalInfo, err := audiocodec.DecodeWAV(historical)
	if err != nil {
		return nil, fmt.Errorf("decode historical output: %w", err)
	}
	currentClip, currentInfo, err := audiocodec.DecodeWAV(current)
	if err != nil {
		return nil, fmt.Errorf("decode current output: %w", err)
	}
	comparison := &OutputComparison{
		HistoricalDigest:      digestOf(historical),
		CurrentDigest:         digestOf(current),
		RawBytesEqual:         digestOf(historical) == digestOf(current),
		HistoricalBytes:       int64(len(historical)),
		CurrentBytes:          int64(len(current)),
		HistoricalFrames:      historicalInfo.FrameCount,
		CurrentFrames:         currentInfo.FrameCount,
		HistoricalChannels:    historicalInfo.Channels,
		CurrentChannels:       currentInfo.Channels,
		HistoricalRate:        historicalInfo.SampleRate,
		CurrentRate:           currentInfo.SampleRate,
	}
	historicalMetrics := audiocodec.Measure(historicalClip.Samples)
	currentMetrics := audiocodec.Measure(currentClip.Samples)
	comparison.HistoricalPeakDBFS = historicalMetrics.PeakDBFS
	comparison.CurrentPeakDBFS = currentMetrics.PeakDBFS
	comparison.HistoricalRMSDBFS = historicalMetrics.RMSDBFS
	comparison.CurrentRMSDBFS = currentMetrics.RMSDBFS
	comparison.PeakDifferenceDB = absFloat(historicalMetrics.PeakDBFS - currentMetrics.PeakDBFS)
	comparison.RMSDifferenceDB = absFloat(historicalMetrics.RMSDBFS - currentMetrics.RMSDBFS)
	if !comparison.GeometryMatches() {
		comparison.Detail = "output geometry differs"
		return comparison, nil
	}
	maxDiff := 0.0
	for index := range historicalClip.Samples {
		diff := absFloat(float64(historicalClip.Samples[index] - currentClip.Samples[index]))
		if diff > maxDiff {
			maxDiff = diff
		}
	}
	comparison.MaxAbsoluteSampleDiff = maxDiff
	comparison.SignalEqual = maxDiff <= tolerance.MaxAbsoluteSampleDiff && comparison.RMSDifferenceDB <= tolerance.MaxRMSDifferenceDB
	return comparison, nil
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

// ReceiptDifferencesOnlyVolatile reports whether every observed receipt
// difference is a field that legitimately moves on a new artifact.
func (report *VerificationReport) ReceiptDifferencesOnlyVolatile() bool {
	for _, difference := range report.ReceiptDifferences {
		if !difference.Volatile {
			return false
		}
	}
	return true
}

// BuildAttestedBundle writes a bundle that records claims (digests, metadata,
// receipt, timeline) but withholds the exact source bytes and output. It is the
// smallest honest hand-off when bytes may not be shared, and Verify must not
// pretend to reproduce anything from it.
func BuildAttestedBundle(outDir string, historical Manifest, timelineRaw, receiptRaw []byte, insight Provenance, now time.Time) (Manifest, error) {
	if outDir == "" {
		return Manifest{}, fmt.Errorf("attested bundle output directory is required")
	}
	if _, err := os.Stat(outDir); err == nil {
		return Manifest{}, fmt.Errorf("bundle %q already exists; bundles are immutable", outDir)
	} else if !os.IsNotExist(err) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(filepath.Dir(outDir), 0o750); err != nil {
		return Manifest{}, err
	}
	temp, err := os.MkdirTemp(filepath.Dir(outDir), ".attested-*")
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		_ = os.RemoveAll(temp)
	}()
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Project:       historical.Project,
		RenderID:      historical.RenderID,
		TimelineID:    historical.TimelineID,
		EvidenceLevel: LevelAttested,
		TimelineFile:  "timeline.json",
		ReceiptFile:   "receipt.json",
		Provenance:    insight,
		CapturedAt:    now.UTC(),
		Sources:       append([]SourceRecord(nil), historical.Sources...),
	}
	for index := range manifest.Sources {
		// An attested bundle deliberately does not ship media bytes; the
		// local paths are blanked so no reader expects them.
		manifest.Sources[index].MediaFile = ""
		manifest.Sources[index].MetaFile = ""
	}
	if err := os.WriteFile(filepath.Join(temp, manifest.TimelineFile), timelineRaw, 0o640); err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(temp, manifest.ReceiptFile), receiptRaw, 0o640); err != nil {
		return Manifest{}, err
	}
	manifest.TimelineDigest = digestOf(timelineRaw)
	manifest.ReceiptDigest = digestOf(receiptRaw)
	records, err := collectFiles(temp, "manifest.json")
	if err != nil {
		return Manifest{}, err
	}
	manifest.Files = records
	canonical, err := canonicalManifest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.ContentDigest = digestOf(canonical)
	final, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(temp, "manifest.json"), append(final, '\n'), 0o640); err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(temp, outDir); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// SortIDs returns the deterministic order used everywhere evidence lists
// sources.
func SortIDs(sources map[string]BundledSource) []string {
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// LoadHistoricalReceipt returns raw and decoded historical receipt bytes.
func LoadHistoricalReceipt(dir string, manifest Manifest) ([]byte, domain.RenderReceipt, error) {
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(manifest.ReceiptFile)))
	if err != nil {
		return nil, domain.RenderReceipt{}, err
	}
	var receipt domain.RenderReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return nil, domain.RenderReceipt{}, err
	}
	return raw, receipt, nil
}

// LoadHistoricalTimeline returns the raw historical timeline bytes.
func LoadHistoricalTimeline(dir string, manifest Manifest) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, filepath.FromSlash(manifest.TimelineFile)))
}

// ProveNoBundleMutation re-loads a bundle and reports whether its content
// digest is unchanged after a verification ran beside it.
func ProveNoBundleMutation(dir, expectedContentDigest string) (bool, error) {
	manifest, err := LoadBundle(dir)
	if err != nil {
		return false, err
	}
	return manifest.ContentDigest == expectedContentDigest, nil
}

// WaitForCaptureClock is a small seam so tests and drills can freeze time
// without touching the service.
func WaitForCaptureClock(_ context.Context, at time.Time) time.Time {
	return at
}
