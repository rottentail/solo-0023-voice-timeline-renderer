package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// AuditFindings is the toolchain-independent half of verification: it checks
// that a bundle's own claims agree with the bytes it ships. It works for any
// producer, including one whose transform contract this verifier cannot run,
// and it never claims reproduction.
type AuditFindings struct {
	BundleContentDigest string        `json:"bundle_content_digest"`
	EvidenceLevel       string        `json:"evidence_level"`
	Passed              bool          `json:"passed"`
	Checks              []CheckResult `json:"checks"`
}

// AuditBundle validates internal consistency without re-rendering.
func AuditBundle(ctx context.Context, bundleDir string) (AuditFindings, error) {
	manifest, err := LoadBundle(bundleDir)
	if err != nil {
		return AuditFindings{}, err
	}
	if err := ctx.Err(); err != nil {
		return AuditFindings{}, err
	}
	findings := AuditFindings{
		BundleContentDigest: manifest.ContentDigest,
		EvidenceLevel:       manifest.EvidenceLevel,
	}
	record := func(name string, passed bool, detail string) {
		findings.Checks = append(findings.Checks, CheckResult{Name: name, Passed: passed, Detail: detail})
	}
	receiptRaw, err := os.ReadFile(filepath.Join(bundleDir, filepath.FromSlash(manifest.ReceiptFile)))
	if err != nil {
		return findings, err
	}
	var receipt domain.RenderReceipt
	if err := json.Unmarshal(receiptRaw, &receipt); err != nil {
		record("receipt-decodes", false, err.Error())
		findings.Passed = false
		return findings, nil
	}
	record("receipt-decodes", true, fmt.Sprintf("schema_version=%d", receipt.SchemaVersion))
	record("receipt-digest-recorded", manifest.ReceiptDigest == digestOf(receiptRaw),
		"manifest receipt digest matches the receipt file")

	// Source claims: every receipt span must point at a source this bundle
	// either ships or describes.
	known := make(map[string]string, len(manifest.Sources))
	for _, source := range manifest.Sources {
		known[source.ID] = source.Digest
	}
	sourceClaims := true
	for _, span := range receipt.Spans {
		digest, ok := known[span.SourceID]
		if !ok {
			record(fmt.Sprintf("receipt-spans[%d]-source-described", span.Index), false,
				fmt.Sprintf("span references %q, which the manifest does not describe", span.SourceID))
			sourceClaims = false
			continue
		}
		if digest != span.SourceDigest {
			record(fmt.Sprintf("receipt-spans[%d]-source-digest", span.Index), false,
				fmt.Sprintf("span cites %s but manifest records %s", span.SourceDigest, digest))
			sourceClaims = false
		}
	}
	record("receipt-source-digests", sourceClaims, "every receipt span cites a manifest source digest")

	if manifest.OutputFile == "" {
		record("output-present", false, "bundle ships no output bytes")
	} else {
		outputBytes, err := os.ReadFile(filepath.Join(bundleDir, filepath.FromSlash(manifest.OutputFile)))
		if err != nil {
			record("output-present", false, err.Error())
			findings.Passed = false
			return findings, nil
		}
		outputDigest := digestOf(outputBytes)
		record("output-digest", outputDigest == receipt.Output.Digest && outputDigest == manifest.OutputDigest,
			fmt.Sprintf("receipt=%s manifest=%s actual=%s", short(receipt.Output.Digest), short(manifest.OutputDigest), short(outputDigest)))
		record("output-byte-count", int64(len(outputBytes)) == receipt.Output.ByteCount,
			fmt.Sprintf("receipt=%d actual=%d", receipt.Output.ByteCount, len(outputBytes)))
		clip, info, err := audiocodec.DecodeWAV(outputBytes)
		if err != nil {
			record("output-decodes", false, err.Error())
		} else {
			record("output-decodes", true, "PCM WAV decodes")
			record("output-frame-count", info.FrameCount == receipt.Output.FrameCount,
				fmt.Sprintf("receipt=%d actual=%d", receipt.Output.FrameCount, info.FrameCount))
			record("output-layout", info.SampleRate == receipt.Output.SampleRate &&
				info.Channels == receipt.Output.Channels && info.BitsPerSample == receipt.Output.BitsPerSample,
				fmt.Sprintf("%d Hz %d ch %d bit", info.SampleRate, info.Channels, info.BitsPerSample))
			measured := audiocodec.Measure(clip.Samples)
			record("output-peak-claim", math.Abs(measured.PeakDBFS-receipt.Output.PeakDBFS) <= 0.25,
				fmt.Sprintf("receipt=%.3f dBFS measured=%.3f dBFS", receipt.Output.PeakDBFS, measured.PeakDBFS))
			record("normalization-target", math.Abs(measured.PeakDBFS-receipt.Normalization.RequestedPeakDBFS) <= 0.25,
				fmt.Sprintf("requested=%.3f dBFS measured=%.3f dBFS", receipt.Normalization.RequestedPeakDBFS, measured.PeakDBFS))
		}
	}
	if manifest.JobFile != "" {
		raw, err := os.ReadFile(filepath.Join(bundleDir, filepath.FromSlash(manifest.JobFile)))
		if err == nil {
			var job domain.RenderJob
			if err := json.Unmarshal(raw, &job); err == nil && job.Output != nil {
				record("job-output-digest", job.Output.Digest == receipt.Output.Digest,
					"job resource and receipt describe the same output")
			}
		}
	}
	findings.Passed = true
	for _, check := range findings.Checks {
		if !check.Passed {
			findings.Passed = false
			break
		}
	}
	return findings, nil
}
