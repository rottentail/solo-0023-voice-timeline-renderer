package repro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
"sort"
	"time"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
)

// LiveEvidence is what a running service produced for one render. It is
// captured from the repository's durable records, not from in-memory state, so
// the bundle reflects exactly what a third party would be served.
type LiveEvidence struct {
	RenderID   string
	Timeline   domain.Timeline
	TimelineRaw []byte
	Sources    map[string]BundledSource
	Receipt    domain.RenderReceipt
	ReceiptRaw []byte
	OutputWAV  []byte
	Job        domain.RenderJob
	JobRaw     []byte
}

// EvidenceReader is the narrow storage surface the capture routine needs. The
// service Repository satisfies it unchanged.
type EvidenceReader interface {
	GetTimeline(string) (domain.Timeline, error)
	GetSource(string) (domain.SourceAsset, []byte, error)
	GetJob(string) (domain.RenderJob, error)
	GetRenderOutput(string) ([]byte, domain.RenderReceipt, error)
}

// CaptureLiveEvidence reads a completed render's evidence from a repository.
func CaptureLiveEvidence(ctx context.Context, reader EvidenceReader, renderID string) (LiveEvidence, error) {
	if err := ctx.Err(); err != nil {
		return LiveEvidence{}, err
	}
	job, err := reader.GetJob(renderID)
	if err != nil {
		return LiveEvidence{}, err
	}
	if job.State != domain.RenderCompleted {
		return LiveEvidence{}, fmt.Errorf("render %q is in state %q; only completed renders have replayable evidence", renderID, job.State)
	}
	timeline, err := reader.GetTimeline(job.TimelineID)
	if err != nil {
		return LiveEvidence{}, err
	}
	timelineRaw, err := marshalEvidence(timeline)
	if err != nil {
		return LiveEvidence{}, err
	}
	jobRaw, err := marshalEvidence(job)
	if err != nil {
		return LiveEvidence{}, err
	}
	sources := make(map[string]BundledSource)
	ids := make([]string, 0, len(timeline.Spans))
	for _, span := range timeline.Spans {
		ids = append(ids, span.SourceID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, done := sources[id]; done {
			continue
		}
		asset, wav, err := reader.GetSource(id)
		if err != nil {
			return LiveEvidence{}, err
		}
		metadataRaw, err := marshalEvidence(asset)
		if err != nil {
			return LiveEvidence{}, err
		}
		sources[id] = BundledSource{Asset: asset, MetadataRaw: metadataRaw, WAV: wav}
	}
	output, receipt, err := reader.GetRenderOutput(renderID)
	if err != nil {
		return LiveEvidence{}, err
	}
	receiptRaw, err := marshalEvidence(receipt)
	if err != nil {
		return LiveEvidence{}, err
	}
	return LiveEvidence{
		RenderID:    renderID,
		Timeline:    timeline,
		TimelineRaw: timelineRaw,
		Sources:     sources,
		Receipt:     receipt,
		ReceiptRaw:  receiptRaw,
		OutputWAV:   output,
		Job:         job,
		JobRaw:      jobRaw,
	}, nil
}

// BuildLiveBundle captures a completed live render and, when trace is
// supplied, records the intermediate stage digests as producer-side evidence.
func BuildLiveBundle(
	ctx context.Context,
	reader EvidenceReader,
	renderID string,
	outDir string,
	provenance Provenance,
	trace *render.Trace,
	now time.Time,
) (Manifest, error) {
	evidence, err := CaptureLiveEvidence(ctx, reader, renderID)
	if err != nil {
		return Manifest{}, err
	}
	options := BundleOptions{
		Project:     "voice-timeline-renderer",
		Timeline:    evidence.Timeline,
		TimelineRaw: evidence.TimelineRaw,
		Sources:     evidence.Sources,
		Receipt:     evidence.Receipt,
		ReceiptRaw:  evidence.ReceiptRaw,
		OutputWAV:   evidence.OutputWAV,
		Job:         &evidence.Job,
		JobRaw:      evidence.JobRaw,
		Provenance:  provenance,
		Now:         now,
	}
	if trace != nil {
		options.Trace = &TraceEnvelope{
			Origin:    "producer",
			Toolchain: provenance.Toolchain,
			Trace:     trace,
		}
	}
	return BuildBundle(outDir, options)
}

func marshalEvidence(value any) ([]byte, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// ReadBundleArtifacts returns the historical output and receipt from a bundle
// without trusting either. Verification uses the manifest file list.
func ReadBundleArtifacts(dir, outputFile, receiptFile string) ([]byte, []byte, error) {
	output, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(outputFile)))
	if err != nil {
		return nil, nil, err
	}
	receipt, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(receiptFile)))
	if err != nil {
		return nil, nil, err
	}
	return output, receipt, nil
}
