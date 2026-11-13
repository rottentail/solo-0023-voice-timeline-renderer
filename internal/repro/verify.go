package repro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
)

// Layer identifiers ordered from earliest to latest in the transform chain.
// Each layer isolates exactly one transform, so the first differing layer is
// the layer where the two runs stopped agreeing.
const (
	LayerManifest    = "manifest-integrity"
	LayerInputs      = "replay-inputs"
	LayerSourceBytes = "source-bytes"
	LayerDecode      = "decode"
	LayerSelection   = "selection"
	LayerResample    = "resample"
	LayerChannelMap  = "channel-map"
	LayerPlacement   = "placement"
	LayerGain        = "gain"
	LayerMix         = "mix"
	LayerNormalize   = "normalization"
	LayerEncoding    = "encoding"
	LayerOutput      = "output-bytes"
	LayerReceipt     = "receipt"
)

var traceLayers = []string{
	LayerSourceBytes,
	LayerDecode,
	LayerSelection,
	LayerResample,
	LayerChannelMap,
	LayerPlacement,
	LayerGain,
	LayerMix,
	LayerNormalize,
	LayerEncoding,
}

// Evidence classes state what a conclusion at a layer may claim. Only
// EvidenceByte supports "the bytes agree"; everything downstream of a float
// transform is EvidenceSignal unless its own digest matched bit-for-bit.
const (
	EvidenceByte   = "byte"
	EvidenceSignal = "signal"
)

const (
	StatusMatch        = "match"
	StatusDiffer       = "differ"
	StatusAbsent       = "absent"
	StatusInconclusive = "inconclusive"
)

// Verdicts are the only conclusions a verification may publish.
const (
	VerdictByteIdentical    = "byte-identical"
	VerdictSampleIdentical  = "sample-identical"
	VerdictSignalEquivalent = "signal-equivalent-within-tolerance"
	VerdictSignalDivergent  = "signal-divergent"
	VerdictIncomparable     = "incomparable"
	VerdictAttestedOnly     = "attested-only-not-reproduced"
)

// SignalTolerance defines what "same signal" means when bytes cannot be
// compared. Peak equality alone is deliberately insufficient.
type SignalTolerance struct {
	MaxAbsoluteSampleDiff float64 `json:"max_absolute_sample_diff"`
	MaxRMSDifferenceDB    float64 `json:"max_rms_difference_db"`
}

func DefaultSignalTolerance() SignalTolerance {
	return SignalTolerance{
		MaxAbsoluteSampleDiff: 1.0 / 32768.0,
		MaxRMSDifferenceDB:    0.05,
	}
}

type LayerResult struct {
	Layer         string `json:"layer"`
	Status        string `json:"status"`
	EvidenceClass string `json:"evidence_class"`
	Historical    string `json:"historical,omitempty"`
	Current       string `json:"current,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

type OutputComparison struct {
	HistoricalDigest      string  `json:"historical_digest,omitempty"`
	CurrentDigest         string  `json:"current_digest,omitempty"`
	RawBytesEqual         bool    `json:"raw_bytes_equal"`
	HistoricalBytes       int64   `json:"historical_byte_count"`
	CurrentBytes          int64   `json:"current_byte_count"`
	HistoricalFrames      int64   `json:"historical_frame_count"`
	CurrentFrames         int64   `json:"current_frame_count"`
	HistoricalChannels    int     `json:"historical_channels"`
	CurrentChannels       int     `json:"current_channels"`
	HistoricalRate        int     `json:"historical_sample_rate"`
	CurrentRate           int     `json:"current_sample_rate"`
	HistoricalPeakDBFS    float64 `json:"historical_peak_dbfs"`
	CurrentPeakDBFS       float64 `json:"current_peak_dbfs"`
	HistoricalRMSDBFS     float64 `json:"historical_rms_dbfs"`
	CurrentRMSDBFS        float64 `json:"current_rms_dbfs"`
	PeakDifferenceDB      float64 `json:"peak_difference_db"`
	RMSDifferenceDB       float64 `json:"rms_difference_db"`
	MaxAbsoluteSampleDiff float64 `json:"max_absolute_sample_diff"`
	SignalEqual           bool    `json:"signal_equal_within_tolerance"`
	Detail                string  `json:"detail,omitempty"`
}

func (comparison *OutputComparison) GeometryMatches() bool {
	return comparison.HistoricalFrames == comparison.CurrentFrames &&
		comparison.HistoricalChannels == comparison.CurrentChannels &&
		comparison.HistoricalRate == comparison.CurrentRate
}

type ReceiptDifference struct {
	Path       string `json:"path"`
	Historical any    `json:"historical"`
	Current    any    `json:"current"`
	Volatile   bool   `json:"volatile"`
}

type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type VerificationReport struct {
	SchemaVersion       int                 `json:"schema_version"`
	BundleDir           string              `json:"bundle_dir"`
	BundleContentDigest string              `json:"bundle_content_digest"`
	RenderID            string              `json:"render_id"`
	TimelineID          string              `json:"timeline_id"`
	EvidenceLevel       string              `json:"evidence_level"`
	Verdict             string              `json:"verdict"`
	EarliestDivergence  string              `json:"earliest_divergence_layer,omitempty"`
	RootCauseLayer      string              `json:"root_cause_layer,omitempty"`
	ByteComparable      bool                `json:"byte_comparable"`
	AlgorithmCompatible bool                `json:"algorithm_compatible"`
	ReplayInputs        string              `json:"replay_inputs"`
	HistoricalToolchain Toolchain           `json:"historical_toolchain"`
	CurrentToolchain    Toolchain           `json:"current_toolchain"`
	Layers              []LayerResult       `json:"layers"`
	Output              *OutputComparison   `json:"output_comparison,omitempty"`
	ReceiptDifferences  []ReceiptDifference `json:"receipt_differences"`
	Checks              []CheckResult       `json:"checks"`
	RecreatedArtifacts  string              `json:"recreated_artifacts,omitempty"`
	Notes               []string            `json:"notes"`
	HistoricalReceiptDigest string          `json:"historical_receipt_digest"`
	RecreatedReceiptDigest  string          `json:"recreated_receipt_digest"`
	VerifiedAt          time.Time           `json:"verified_at"`
}

// ReplayInputs lets a verifier render from substitute inputs while the
// historical evidence stays untouched. This models a third party whose local
// source bytes or timeline file differ from the producer's.
type ReplayInputs struct {
	// Label is required whenever any override is set, so a report can never
	// blur "same inputs" with "substituted inputs".
	Label         string
	Timeline      []byte
	SourceMedia   map[string][]byte
	SourceMetadata map[string][]byte
}

func (inputs ReplayInputs) Active() bool {
	return len(inputs.Timeline) > 0 || len(inputs.SourceMedia) > 0 || len(inputs.SourceMetadata) > 0
}

type VerifyOptions struct {
	CapturedBy      string
	Toolchain       Toolchain
	Limits          render.Limits
	Inputs          ReplayInputs
	WriteRecreated  bool
	RecreatedDir    string
	Now             time.Time
	SignalTolerance *SignalTolerance
}

// Verify replays the bundle's inputs through the current pipeline and compares
// the result with the preserved historical evidence. It never writes into the
// bundle directory and never rewrites the historical receipt.
func Verify(ctx context.Context, bundleDir string, options VerifyOptions) (VerificationReport, error) {
	manifest, err := LoadBundle(bundleDir)
	if err != nil {
		return VerificationReport{}, err
	}
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	tolerance := DefaultSignalTolerance()
	if options.SignalTolerance != nil {
		tolerance = *options.SignalTolerance
	}
	if options.Toolchain.AlgorithmDigest == "" {
		options.Toolchain = CurrentToolchain("voice-timeline-renderer", "", "", false)
	}
	if options.Inputs.Active() && options.Inputs.Label == "" {
		return VerificationReport{}, errors.New("substituted replay inputs must be labelled")
	}
	report := VerificationReport{
		SchemaVersion:           SchemaVersion,
		BundleDir:               bundleDir,
		BundleContentDigest:     manifest.ContentDigest,
		RenderID:                manifest.RenderID,
		TimelineID:              manifest.TimelineID,
		EvidenceLevel:           manifest.EvidenceLevel,
		ReplayInputs:            "historical",
		HistoricalToolchain:     manifest.Provenance.Toolchain,
		CurrentToolchain:        options.Toolchain,
		HistoricalReceiptDigest: manifest.ReceiptDigest,
		VerifiedAt:              options.Now.UTC(),
	}
	if options.Inputs.Active() {
		report.ReplayInputs = options.Inputs.Label
		report.Notes = append(report.Notes, "replay used substituted inputs ("+options.Inputs.Label+"); historical evidence was not modified")
	}
	report.AlgorithmCompatible = manifest.Provenance.Toolchain.AlgorithmDigest == "" ||
		manifest.Provenance.Toolchain.AlgorithmDigest == options.Toolchain.AlgorithmDigest
	report.AddLayer(LayerManifest, StatusMatch, EvidenceByte, manifest.ContentDigest, manifest.ContentDigest,
		"manifest and every referenced file match their recorded digests")
	report.AddLayer(LayerInputs, StatusMatch, EvidenceByte, "historical", report.ReplayInputs, "which inputs produced the compared run")

	if manifest.EvidenceLevel != LevelReplayable {
		report.Verdict = VerdictAttestedOnly
		report.Notes = append(report.Notes, "attested bundles carry no exact source bytes; nothing was re-produced")
		report.Checks = append(report.Checks, CheckResult{Name: "replay-possible", Passed: false, Detail: "bundle is attested, not replayable"})
		return report, nil
	}

	timelineRaw, err := os.ReadFile(filepath.Join(bundleDir, manifest.TimelineFile))
	if err != nil {
		return report, err
	}
	if len(options.Inputs.Timeline) > 0 {
		timelineRaw = options.Inputs.Timeline
	}
	var plan domain.Timeline
	if err := json.Unmarshal(timelineRaw, &plan); err != nil {
		return report, fmt.Errorf("decode timeline evidence: %w", err)
	}
	sources := make(map[string]render.SourceInput, len(manifest.Sources))
	sourceAssets := make(map[string]domain.SourceAsset, len(manifest.Sources))
	for _, record := range manifest.Sources {
		var asset domain.SourceAsset
		metadataRaw, err := os.ReadFile(filepath.Join(bundleDir, record.MetaFile))
		if err != nil {
			return report, err
		}
		if err := json.Unmarshal(metadataRaw, &asset); err != nil {
			return report, fmt.Errorf("decode source metadata for %q: %w", record.ID, err)
		}
		wav, err := os.ReadFile(filepath.Join(bundleDir, record.MediaFile))
		if err != nil {
			return report, err
		}
		if override, ok := options.Inputs.SourceMedia[record.ID]; ok {
			wav = override
			}
		if override, ok := options.Inputs.SourceMetadata[record.ID]; ok {
			if err := json.Unmarshal(override, &asset); err != nil {
				return report, fmt.Errorf("decode override metadata for %q: %w", record.ID, err)
			}
		} else if _, substituted := options.Inputs.SourceMedia[record.ID]; substituted {
			fresh, err := assetFromMedia(record.ID, asset.Name, wav)
			if err != nil {
				return report, fmt.Errorf("substituted source %q is not a valid source: %w", record.ID, err)
			}
			asset = fresh
		}
		sources[record.ID] = render.SourceInput{Asset: asset, WAV: wav}
		sourceAssets[record.ID] = asset
	}
	maxFrames := options.Limits.MaxOutputFrames
	if maxFrames <= 0 {
		maxFrames = math.MaxInt64
	}
	if err := domain.ValidateTimeline(plan, sourceAssets, len(plan.Spans), maxFrames); err != nil {
		report.AddLayer(LayerSelection, StatusDiffer, EvidenceByte, "", "", "replay timeline is invalid against the supplied sources: "+err.Error())
		report.Verdict = VerdictIncomparable
		report.Notes = append(report.Notes, "the replay plan does not satisfy service validation, so no output was produced")
		report.Checks = append(report.Checks, CheckResult{Name: "replay-plan-valid", Passed: false, Detail: err.Error()})
		return report, nil
	}
	report.Checks = append(report.Checks, CheckResult{Name: "replay-plan-valid", Passed: true, Detail: "replay plan passes the same validation as a live write"})

	var historicalTrace *render.Trace
	if manifest.TraceFile != "" {
		raw, err := os.ReadFile(filepath.Join(bundleDir, manifest.TraceFile))
		if err != nil {
			return report, err
		}
		var envelope struct {
			Origin    string        `json:"origin"`
			Toolchain Toolchain     `json:"toolchain"`
			Trace     *render.Trace `json:"trace"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return report, fmt.Errorf("decode trace evidence: %w", err)
		}
		historicalTrace = envelope.Trace
	} else {
		report.Notes = append(report.Notes,
			"bundle carries no stage trace: divergence can be located at the published output but cannot be attributed to an earlier transform layer")
	}

	currentTrace := &render.Trace{}
	result, renderErr := render.RenderWithTrace(ctx, manifest.RenderID, plan, sources, options.Limits, options.Now, currentTrace)
	if renderErr != nil {
		report.Verdict = VerdictIncomparable
		report.Notes = append(report.Notes, "replay failed: "+renderErr.Error())
		report.Checks = append(report.Checks, CheckResult{Name: "replay-completed", Passed: false, Detail: renderErr.Error()})
		return report, nil
	}
	report.Checks = append(report.Checks, CheckResult{Name: "replay-completed", Passed: true, Detail: "pipeline produced an output"})
	report.RecreatedReceiptDigest = receiptDigest(result.Receipt)

	report.compareTrace(historicalTrace, currentTrace)
	report.compareOutput(bundleDir, manifest, result, tolerance)
	report.compareReceipt(bundleDir, manifest, result.Receipt)

	if options.WriteRecreated {
		if err := writeRecreated(options.RecreatedDir, result); err != nil {
			return report, err
		}
		report.RecreatedArtifacts = options.RecreatedDir
	}
	report.decide()
	return report, nil
}

// assetFromMedia derives source facts from substituted bytes exactly the way a
// fresh registration would, so a substitution cannot smuggle stale metadata.
func assetFromMedia(id, name string, wav []byte) (domain.SourceAsset, error) {
	clip, info, err := audiocodec.DecodeWAV(wav)
	if err != nil {
		return domain.SourceAsset{}, err
	}
	facts := domain.SourceFacts{
		SampleRate: info.SampleRate, Channels: info.Channels,
		BitsPerSample: info.BitsPerSample, FrameCount: info.FrameCount,
	}
	if err := domain.ValidateSourceFacts("source", facts); err != nil {
		return domain.SourceAsset{}, err
	}
	measurements := audiocodec.Measure(clip.Samples)
	return domain.SourceAsset{
		ID: id, Name: name,
		Digest:     digestOf(wav),
		ByteCount:  info.ByteCount,
		SampleRate: info.SampleRate, Channels: info.Channels,
		BitsPerSample: info.BitsPerSample, FrameCount: info.FrameCount,
		DurationMS: info.FrameCount * 1000 / int64(info.SampleRate),
		PeakDBFS:   measurements.PeakDBFS, RMSDBFS: measurements.RMSDBFS,
	}, nil
}

func receiptDigest(receipt domain.RenderReceipt) string {
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return ""
	}
	return digestOf(encoded)
}

func (report *VerificationReport) AddLayer(layer, status, evidence, historical, current, detail string) {
	report.Layers = append(report.Layers, LayerResult{
		Layer: layer, Status: status, EvidenceClass: evidence,
		Historical: historical, Current: current, Detail: detail,
	})
}

func (report *VerificationReport) compareTrace(historical, current *render.Trace) {
	if historical == nil {
		for _, layer := range traceLayers {
			report.AddLayer(layer, StatusAbsent, EvidenceSignal, "", "", "no historical stage trace")
		}
		return
	}
	status := func(equal bool) string {
		if equal {
			return StatusMatch
		}
		if !report.AlgorithmCompatible {
			return StatusInconclusive
		}
		return StatusDiffer
	}
	sourceStatus, hs, cs, detail := sourceLayerStatus(historical, current)
	report.AddLayer(LayerSourceBytes, sourceStatus, EvidenceByte, hs, cs, detail)
	layerPairs := []struct {
		layer     string
		equal     bool
		hist, cur string
	}{
		{LayerDecode, decodeEqual(historical, current), decodeSummary(historical), decodeSummary(current)},
		{LayerSelection, selectionEqual(historical, current), selectionSummary(historical), selectionSummary(current)},
		{LayerResample, resampleEqual(historical, current), resampleSummary(historical), resampleSummary(current)},
		{LayerChannelMap, channelMapEqual(historical, current), channelMapSummary(historical), channelMapSummary(current)},
		{LayerPlacement, placementEqual(historical, current), placementSummary(historical), placementSummary(current)},
		{LayerGain, gainEqual(historical, current), gainSummary(historical), gainSummary(current)},
		{LayerMix, mixEqual(historical, current), mixSummary(historical), mixSummary(current)},
		{LayerNormalize, normalizeEqual(historical, current), normalizeSummary(historical), normalizeSummary(current)},
		{LayerEncoding, encodingEqual(historical, current), encodingSummary(historical), encodingSummary(current)},
	}
	for _, pair := range layerPairs {
		report.AddLayer(pair.layer, status(pair.equal), traceEvidenceClass(pair.layer), pair.hist, pair.cur, "")
	}
}

// traceEvidenceClass states what the digest for a layer may prove. Source
// bytes and encoded output are byte facts; everything between them is a float
// signal, so equality there is bit-identical floats, not a byte guarantee.
func traceEvidenceClass(layer string) string {
	switch layer {
	case LayerSourceBytes, LayerEncoding:
		return EvidenceByte
	default:
		return EvidenceSignal
	}
}

func sourceLayerStatus(historical, current *render.Trace) (string, string, string, string) {
	if len(historical.Sources) != len(current.Sources) {
		return StatusDiffer, "", "", "source set size changed"
	}
	for index := range historical.Sources {
		a := historical.Sources[index]
		b := current.Sources[index]
		if a.SourceID != b.SourceID {
			return StatusDiffer, a.SourceID, b.SourceID, "source order or identity changed"
		}
		if a.BytesDigest != b.BytesDigest {
			return StatusDiffer, a.SourceID + ":" + a.BytesDigest, b.SourceID + ":" + b.BytesDigest,
				"source media bytes differ for " + a.SourceID
		}
	}
	return StatusMatch, sourceSummary(historical), sourceSummary(current), ""
}

func decodeEqual(a, b *render.Trace) bool {
	return sameSourceField(a, b, func(s render.SourceTrace) string {
		return fmt.Sprint(s.PCMDigest, s.DecodedFrames, s.SampleRate, s.Channels)
	})
}
func selectionEqual(a, b *render.Trace) bool {
	return sameSpanField(a, b, func(s render.SpanTrace) string {
		return fmt.Sprint(s.SourceStartFrame, s.SourceEndFrame, s.TargetSampleRate)
	})
}
func resampleEqual(a, b *render.Trace) bool {
	return sameSpanField(a, b, func(s render.SpanTrace) string {
		return fmt.Sprint(s.ResampledFrames, s.ResampledChannels, s.ResampledDigest)
	})
}
func channelMapEqual(a, b *render.Trace) bool {
	return sameSpanField(a, b, func(s render.SpanTrace) string {
		return fmt.Sprint(s.ChannelAction, s.MappedFrames, s.MappedDigest)
	})
}
func placementEqual(a, b *render.Trace) bool {
	return sameSpanField(a, b, func(s render.SpanTrace) string {
		return fmt.Sprint(s.OutputStartFrame, s.OutputFrames, s.CrossfadeFrames)
	})
}
func gainEqual(a, b *render.Trace) bool {
	return sameSpanField(a, b, func(s render.SpanTrace) string { return fmt.Sprint(s.GainDB, s.GainedDigest) })
}
func mixEqual(a, b *render.Trace) bool {
	return fmt.Sprint(a.Mix) == fmt.Sprint(b.Mix)
}
func normalizeEqual(a, b *render.Trace) bool {
	return fmt.Sprint(a.Normalization) == fmt.Sprint(b.Normalization)
}
func encodingEqual(a, b *render.Trace) bool {
	return fmt.Sprint(a.Encoding) == fmt.Sprint(b.Encoding)
}

func sameSourceField(a, b *render.Trace, selectField func(render.SourceTrace) string) bool {
	if len(a.Sources) != len(b.Sources) {
		return false
	}
	for index := range a.Sources {
		if a.Sources[index].SourceID != b.Sources[index].SourceID {
			return false
		}
		if selectField(a.Sources[index]) != selectField(b.Sources[index]) {
			return false
		}
	}
	return true
}

func sameSpanField(a, b *render.Trace, selectField func(render.SpanTrace) string) bool {
	if len(a.Spans) != len(b.Spans) {
		return false
	}
	for index := range a.Spans {
		if selectField(a.Spans[index]) != selectField(b.Spans[index]) {
			return false
		}
	}
	return true
}

func sourceSummary(trace *render.Trace) string {
	return joinSource(trace, func(s render.SourceTrace) string { return s.SourceID + ":" + s.BytesDigest })
}
func decodeSummary(trace *render.Trace) string {
	return joinSource(trace, func(s render.SourceTrace) string {
		return fmt.Sprintf("%s:%d@%d/%d:%s", s.SourceID, s.DecodedFrames, s.SampleRate, s.Channels, s.PCMDigest)
	})
}
func selectionSummary(trace *render.Trace) string {
	return joinSpan(trace, func(s render.SpanTrace) string {
		return fmt.Sprintf("%d:%d-%d@%d", s.Index, s.SourceStartFrame, s.SourceEndFrame, s.TargetSampleRate)
	})
}
func resampleSummary(trace *render.Trace) string {
	return joinSpan(trace, func(s render.SpanTrace) string {
		return fmt.Sprintf("%d:%d/%d:%s", s.Index, s.ResampledFrames, s.ResampledChannels, s.ResampledDigest)
	})
}
func channelMapSummary(trace *render.Trace) string {
	return joinSpan(trace, func(s render.SpanTrace) string {
		return fmt.Sprintf("%d:%s:%d:%s", s.Index, s.ChannelAction, s.MappedFrames, s.MappedDigest)
	})
}
func placementSummary(trace *render.Trace) string {
	return joinSpan(trace, func(s render.SpanTrace) string {
		return fmt.Sprintf("%d:%d+%d:x%d", s.Index, s.OutputStartFrame, s.OutputFrames, s.CrossfadeFrames)
	})
}
func gainSummary(trace *render.Trace) string {
	return joinSpan(trace, func(s render.SpanTrace) string { return fmt.Sprintf("%d:g%.3f:%s", s.Index, s.GainDB, s.GainedDigest) })
}
func mixSummary(trace *render.Trace) string {
	return fmt.Sprintf("%d/%d:%s:peak%.3f:rms%.3f", trace.Mix.Frames, trace.Mix.Channels, trace.Mix.Digest, trace.Mix.PeakDBFS, trace.Mix.RMSDBFS)
}
func normalizeSummary(trace *render.Trace) string {
	return fmt.Sprintf("before%.3f:gain%.3f:after%.3f:%s", trace.Normalization.BeforePeakDBFS, trace.Normalization.AppliedGainDB, trace.Normalization.AfterPeakDBFS, trace.Normalization.Digest)
}
func encodingSummary(trace *render.Trace) string {
	return fmt.Sprintf("%d/%d/%d:%d:%d:%s", trace.Encoding.SampleRate, trace.Encoding.Channels, trace.Encoding.BitsPerSample, trace.Encoding.Frames, trace.Encoding.ByteCount, trace.Encoding.Digest)
}

func joinSource(trace *render.Trace, format func(render.SourceTrace) string) string {
	parts := make([]string, 0, len(trace.Sources))
	for _, source := range trace.Sources {
		parts = append(parts, format(source))
	}
	return strings.Join(parts, ",")
}

func joinSpan(trace *render.Trace, format func(render.SpanTrace) string) string {
	parts := make([]string, 0, len(trace.Spans))
	for _, span := range trace.Spans {
		parts = append(parts, format(span))
	}
	return strings.Join(parts, ",")
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func (report *VerificationReport) compareOutput(bundleDir string, manifest Manifest, result render.Result, tolerance SignalTolerance) {
	if manifest.OutputFile == "" {
		report.AddLayer(LayerOutput, StatusAbsent, EvidenceByte, "", "", "bundle does not carry output bytes")
		return
	}
	historicalBytes, err := os.ReadFile(filepath.Join(bundleDir, manifest.OutputFile))
	if err != nil {
		report.AddLayer(LayerOutput, StatusAbsent, EvidenceByte, "", "", err.Error())
		return
	}
	comparison := &OutputComparison{
		HistoricalDigest: digestOf(historicalBytes),
		CurrentDigest:    digestOf(result.WAV),
		RawBytesEqual:    digestOf(historicalBytes) == digestOf(result.WAV),
		HistoricalBytes:  int64(len(historicalBytes)),
		CurrentBytes:     int64(len(result.WAV)),
	}
	report.Output = comparison
	historicalClip, historicalInfo, historicalErr := audiocodec.DecodeWAV(historicalBytes)
	currentClip, currentInfo, currentErr := audiocodec.DecodeWAV(result.WAV)
	if historicalErr != nil || currentErr != nil {
		report.AddLayer(LayerOutput, StatusInconclusive, EvidenceSignal, comparison.HistoricalDigest, comparison.CurrentDigest,
			fmt.Sprintf("cannot decode one side: historical=%v current=%v", historicalErr, currentErr))
		report.Checks = append(report.Checks, CheckResult{Name: "output-decodes", Passed: false, Detail: "one or both outputs failed to decode"})
		return
	}
	report.Checks = append(report.Checks, CheckResult{Name: "output-decodes", Passed: true, Detail: "both outputs decode as PCM WAV"})

	comparison.HistoricalFrames = historicalInfo.FrameCount
	comparison.CurrentFrames = currentInfo.FrameCount
	comparison.HistoricalChannels = historicalInfo.Channels
	comparison.CurrentChannels = currentInfo.Channels
	comparison.HistoricalRate = historicalInfo.SampleRate
	comparison.CurrentRate = currentInfo.SampleRate
	historicalMetrics := audiocodec.Measure(historicalClip.Samples)
	currentMetrics := audiocodec.Measure(currentClip.Samples)
	comparison.HistoricalPeakDBFS = historicalMetrics.PeakDBFS
	comparison.CurrentPeakDBFS = currentMetrics.PeakDBFS
	comparison.HistoricalRMSDBFS = historicalMetrics.RMSDBFS
	comparison.CurrentRMSDBFS = currentMetrics.RMSDBFS
	comparison.PeakDifferenceDB = math.Abs(historicalMetrics.PeakDBFS - currentMetrics.PeakDBFS)
	comparison.RMSDifferenceDB = math.Abs(historicalMetrics.RMSDBFS - currentMetrics.RMSDBFS)

	if !comparison.GeometryMatches() {
		comparison.Detail = "output geometry differs; no per-sample signal comparison is possible"
	} else {
		maxDiff := 0.0
		for index := range historicalClip.Samples {
			diff := math.Abs(float64(historicalClip.Samples[index]) - float64(currentClip.Samples[index]))
			if diff > maxDiff {
				maxDiff = diff
			}
		}
		comparison.MaxAbsoluteSampleDiff = maxDiff
		comparison.SignalEqual = maxDiff <= tolerance.MaxAbsoluteSampleDiff &&
			comparison.RMSDifferenceDB <= tolerance.MaxRMSDifferenceDB
		switch {
		case comparison.RawBytesEqual:
			comparison.Detail = "published output bytes are identical"
		case comparison.SignalEqual:
			comparison.Detail = "container or quantization bytes differ while the decoded signal stays within tolerance"
		default:
			comparison.Detail = "decoded signal differs beyond tolerance"
		}
	}

	// The explicit anti-pattern the convention forbids: equal normalized peak
	// while the signal differs.
	peakOnlyRisk := comparison.PeakDifferenceDB <= tolerance.MaxRMSDifferenceDB && !comparison.SignalEqual
	report.Checks = append(report.Checks, CheckResult{
		Name:   "peak-equality-is-not-equivalence",
		Passed: !peakOnlyRisk,
		Detail: peakOnlyExplanation(comparison, peakOnlyRisk),
	})
	switch {
	case comparison.RawBytesEqual:
		report.AddLayer(LayerOutput, StatusMatch, EvidenceByte, comparison.HistoricalDigest, comparison.CurrentDigest, comparison.Detail)
	case comparison.SignalEqual:
		report.AddLayer(LayerOutput, StatusMatch, EvidenceSignal, comparison.HistoricalDigest, comparison.CurrentDigest, comparison.Detail)
	default:
		report.AddLayer(LayerOutput, StatusDiffer, EvidenceSignal, comparison.HistoricalDigest, comparison.CurrentDigest, comparison.Detail)
	}
}

func peakOnlyExplanation(comparison *OutputComparison, risk bool) string {
	if risk {
		return fmt.Sprintf("peaks differ by only %.3f dB while samples differ by %.4f (%d frames): matching normalized peak is not audio equivalence",
			comparison.PeakDifferenceDB, comparison.MaxAbsoluteSampleDiff, comparison.HistoricalFrames)
	}
	return fmt.Sprintf("peak delta %.3f dB, RMS delta %.3f dB, max sample delta %.6f",
		comparison.PeakDifferenceDB, comparison.RMSDifferenceDB, comparison.MaxAbsoluteSampleDiff)
}

func (report *VerificationReport) compareReceipt(bundleDir string, manifest Manifest, current domain.RenderReceipt) {
	historicalRaw, err := os.ReadFile(filepath.Join(bundleDir, manifest.ReceiptFile))
	if err != nil {
		report.AddLayer(LayerReceipt, StatusAbsent, EvidenceByte, "", "", err.Error())
		return
	}
	var historical map[string]any
	if err := json.Unmarshal(historicalRaw, &historical); err != nil {
		report.AddLayer(LayerReceipt, StatusInconclusive, EvidenceByte, "", "", err.Error())
		return
	}
	currentEncoded, err := json.Marshal(current)
	if err != nil {
		return
	}
	var currentMap map[string]any
	if err := json.Unmarshal(currentEncoded, &currentMap); err != nil {
		return
	}
	historicalFlat := flatten("", historical)
	currentFlat := flatten("", currentMap)
	paths := make([]string, 0, len(historicalFlat))
	for path := range historicalFlat {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		before := historicalFlat[path]
		after, ok := currentFlat[path]
		if !ok {
			report.ReceiptDifferences = append(report.ReceiptDifferences, ReceiptDifference{Path: path, Historical: before, Current: nil, Volatile: true})
			continue
		}
		if fmt.Sprint(before) == fmt.Sprint(after) {
			continue
		}
		report.ReceiptDifferences = append(report.ReceiptDifferences, ReceiptDifference{
			Path: path, Historical: before, Current: after, Volatile: volatileReceiptPath(path),
		})
	}
	for path := range currentFlat {
		if _, ok := historicalFlat[path]; !ok {
			report.ReceiptDifferences = append(report.ReceiptDifferences, ReceiptDifference{Path: path, Current: currentFlat[path]})
		}
	}
	// A re-render is a new artifact: created_at, render_id, and the output
	// digest legitimately move. Everything describing the assembled signal must
	// not. Volatile fields are reported but never treated as equivalence.
	meaningful, volatile := 0, 0
	for _, difference := range report.ReceiptDifferences {
		if difference.Volatile {
			volatile++
		} else {
			meaningful++
		}
	}
	status, detail := StatusMatch, "historical receipt matches the re-created receipt on every semantic field"
	if meaningful > 0 {
		status = StatusDiffer
		detail = fmt.Sprintf("%d semantic receipt field(s) differ, %d volatile field(s) differ", meaningful, volatile)
	} else if volatile > 0 {
		detail = fmt.Sprintf("only %d volatile field(s) differ; the historical receipt itself is preserved unchanged", volatile)
	}
	report.AddLayer(LayerReceipt, status, EvidenceByte, manifest.ReceiptDigest, receiptDigest(current), detail)
}

func volatileReceiptPath(path string) bool {
	// Only fields that carry no signal information may move. A changed output
	// digest, byte count, peak, RMS, span placement, or normalization figure
	// is always a semantic difference, even when a re-render is legitimate.
	return path == "created_at"
}

func flatten(prefix string, value any) map[string]any {
	result := make(map[string]any)
	flattenInto(result, prefix, value)
	return result
}

func flattenInto(result map[string]any, path string, value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			flattenInto(result, joinPath(path, key), child)
		}
	case []any:
		for index, child := range typed {
			flattenInto(result, fmt.Sprintf("%s[%d]", path, index), child)
		}
	default:
		if path != "" {
			result[path] = value
		}
	}
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func (report *VerificationReport) decide() {
	earliest, root := "", ""
	for _, layer := range report.Layers {
		if layer.Status != StatusDiffer && layer.Status != StatusInconclusive {
			continue
		}
		if !isTraceLayer(layer.Layer) {
			continue
		}
		if earliest == "" {
			earliest = layer.Layer
		}
		if root == "" && layer.Status == StatusDiffer {
			root = layer.Layer
		}
	}
	report.EarliestDivergence = earliest
	report.RootCauseLayer = root
	report.ByteComparable = report.AlgorithmCompatible

	if earliest != "" && !report.AlgorithmCompatible {
		report.Verdict = VerdictIncomparable
		report.Notes = append(report.Notes,
			"a stage digest differs and the algorithm contract also changed, so byte-level attribution is unavailable; the decoded-signal comparison is the only admissible conclusion")
		return
	}
	if earliest != "" {
		report.Verdict = VerdictSignalDivergent
		return
	}
	if report.Output == nil {
		report.Verdict = VerdictIncomparable
		report.Notes = append(report.Notes, "bundle has no output bytes; only stage traces were compared")
				return
	}
	if report.Output.RawBytesEqual {
		report.Verdict = VerdictByteIdentical
		return
	}
	if !report.Output.SignalEqual {
		report.Verdict = VerdictSignalDivergent
		if report.EarliestDivergence == "" {
			report.Notes = append(report.Notes, "the outputs differ but no trace layer was recorded, so the responsible layer cannot be named")
		}
		return
	}
	if report.Output.GeometryMatches() && report.Output.MaxAbsoluteSampleDiff == 0 {
		report.Verdict = VerdictSampleIdentical
		return
	}
	report.Verdict = VerdictSignalEquivalent
}

func isTraceLayer(layer string) bool {
	for _, candidate := range traceLayers {
		if candidate == layer {
			return true
		}
	}
	return false
}

func writeRecreated(dir string, result render.Result) error {
	if dir == "" {
		return errors.New("recreated artifact directory is required when writing recreated artifacts")
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("recreated directory %q already exists; derived artifacts never overwrite history", dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(dir), ".recreated-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = os.RemoveAll(temp)
	}()
	if err := os.WriteFile(filepath.Join(temp, "output.wav"), result.WAV, 0o640); err != nil {
		return err
	}
	receiptRaw, err := json.MarshalIndent(result.Receipt, "", "  ")
	if err != nil {
		return err
	}
	receiptRaw = append(receiptRaw, '\n')
	if err := os.WriteFile(filepath.Join(temp, "receipt.json"), receiptRaw, 0o640); err != nil {
		return err
	}
	if err := os.Rename(temp, dir); err != nil {
		return err
	}
	return nil
}
