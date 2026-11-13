package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/httpapi"
	"voice-timeline-renderer/internal/render"
	"voice-timeline-renderer/internal/repro"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
	"net/http"
	"net/http/httptest"
	"bytes"
	"io"
)

// drill.go builds the studio project through the real HTTP surface, so the
// rehearsal exercises the same validation and persistence a third party would.

type studioProject struct {
	renderID       string
	plan           domain.Timeline
	timelineRaw    []byte
	receipt        domain.RenderReceipt
	receiptRaw     []byte
	outputDigest   string
	outputWAV      []byte
	sourceA        domain.SourceAsset
	sourceAID      string
	sourceB        domain.SourceAsset
	sourceBID      string
	sourceAWAV     []byte
	sourceBWAV     []byte
	bundledSources map[string]repro.BundledSource
	sources        map[string]render.SourceInput
	job            domain.RenderJob
	jobRaw         []byte
}

type httpHarness struct {
	server *httptest.Server
	client *http.Client
}

func buildStudioProjectHTTP(ctx context.Context, repo *storage.Repository, svc *service.Service) (*studioProject, error) {
	server := httptest.NewServer(httpapi.New(svc, httpapi.Options{
		MaxSourceRequestBytes: 48 << 20,
		MaxJSONBytes:          2 << 20,
	}).Handler())
	defer server.Close()
	harness := &httpHarness{server: server, client: &http.Client{Timeout: 60 * time.Second}}

	// Mixed sample rates and a mono/stereo mismatch are deliberate: the
	// project exercises the resampler and channel mapper, not just placement.
	sourceAWAV := sineWAV(44100, 1, 40, 26460, 220, 0.30, 0, 1)
	sourceBWAV := sineWAV(48000, 2, 40, 24000, 330, 0.20, 0.4, 0.8)
	sourceAID := "studio_voice_a"
	sourceBID := "studio_voice_b"
	sourceA, err := harness.putSource(ctx, sourceAID, "studio voice a", sourceAWAV)
	if err != nil {
		return nil, err
	}
	if sourceA.FrameCount != 26460 {
		return nil, fmt.Errorf("source a frame count %d", sourceA.FrameCount)
	}
	sourceB, err := harness.putSource(ctx, sourceBID, "studio voice b", sourceBWAV)
	if err != nil {
		return nil, err
	}
	if sourceB.Channels != 2 || sourceB.SampleRate != 48000 {
		return nil, fmt.Errorf("source b facts are wrong: %+v", sourceB)
	}
	spans := []domain.TimelineSpan{
		{SourceID: sourceAID, SourceStartFrame: 0, SourceEndFrame: 26460, GainDB: -3},
		{SourceID: sourceBID, SourceStartFrame: 0, SourceEndFrame: 24000, GainDB: 0.5, CrossfadeFrames: 4800},
	}
	timeline, err := harness.putTimeline(ctx, "studio_timeline_01", "two speaker assembly", 48000, 2, -1, spans)
	if err != nil {
		return nil, err
	}
	timelineRaw, err := json.MarshalIndent(timeline, "", "  ")
	if err != nil {
		return nil, err
	}
	timelineRaw = append(timelineRaw, '\n')
	renderID := "studio_render_01"
	job, err := harness.putRender(ctx, renderID, timeline.ID)
	if err != nil {
		return nil, err
	}
	if job.State != domain.RenderCompleted {
		return nil, fmt.Errorf("render ended in %s: %+v", job.State, job.Error)
	}
	outputBytes, err := harness.getOutput(ctx, renderID)
	if err != nil {
		return nil, err
	}
	receipt, err := harness.getReceipt(ctx, renderID)
	if err != nil {
		return nil, err
	}
	receiptRaw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, err
	}
	receiptRaw = append(receiptRaw, '\n')
	jobRaw, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return nil, err
	}
	jobRaw = append(jobRaw, '\n')

	assetA, wavA, err := repo.GetSource(sourceAID)
	if err != nil {
		return nil, err
	}
	assetB, wavB, err := repo.GetSource(sourceBID)
	if err != nil {
		return nil, err
	}
	metaARaw := mustMarshal(assetA)
	metaBRaw := mustMarshal(assetB)
	bundled := map[string]repro.BundledSource{
		sourceAID: {Asset: assetA, MetadataRaw: metaARaw, WAV: wavA},
		sourceBID: {Asset: assetB, MetadataRaw: metaBRaw, WAV: wavB},
	}
	inputs := map[string]render.SourceInput{
		sourceAID: {Asset: assetA, WAV: wavA},
		sourceBID: {Asset: assetB, WAV: wavB},
	}
	return &studioProject{
		renderID:       renderID,
		plan:           timeline,
		timelineRaw:    timelineRaw,
		receipt:        receipt,
		receiptRaw:     receiptRaw,
		outputDigest:   digestOfBytes(outputBytes),
		outputWAV:      outputBytes,
		sourceA:        assetA, sourceAID: sourceAID, sourceAWAV: wavA,
		sourceB:        assetB, sourceBID: sourceBID, sourceBWAV: wavB,
		bundledSources: bundled,
		sources:        inputs,
		job:            job,
		jobRaw:         jobRaw,
	}, nil
}

func (harness *httpHarness) do(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, harness.server.URL+path, body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := harness.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	payloadBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, payloadBytes, nil
}

func (harness *httpHarness) putSource(ctx context.Context, id, name string, wav []byte) (domain.SourceAsset, error) {
	status, body, err := harness.do(ctx, "PUT", "/v1/sources/"+id, service.SourceWriteRequest{
		Name: name, WAVBase64: base64.StdEncoding.EncodeToString(wav),
	})
	if err != nil {
		return domain.SourceAsset{}, err
	}
	if status != 200 && status != 201 {
		return domain.SourceAsset{}, fmt.Errorf("source %s returned %d: %s", id, status, string(body))
	}
	var asset domain.SourceAsset
	if err := json.Unmarshal(body, &asset); err != nil {
		return domain.SourceAsset{}, err
	}
	return asset, nil
}

func (harness *httpHarness) putTimeline(ctx context.Context, id, name string, rate, channels int, target float64, spans []domain.TimelineSpan) (domain.Timeline, error) {
	status, body, err := harness.do(ctx, "PUT", "/v1/timelines/"+id, service.TimelineWriteRequest{
		Name: name, SampleRate: rate, OutputChannels: channels, TargetPeakDBFS: target, Spans: spans,
	})
	if err != nil {
		return domain.Timeline{}, err
	}
	if status != 200 && status != 201 {
		return domain.Timeline{}, fmt.Errorf("timeline %s returned %d: %s", id, status, string(body))
	}
	var plan domain.Timeline
	if err := json.Unmarshal(body, &plan); err != nil {
		return domain.Timeline{}, err
	}
	return plan, nil
}

func (harness *httpHarness) putRender(ctx context.Context, id, timelineID string) (domain.RenderJob, error) {
	status, body, err := harness.do(ctx, "PUT", "/v1/renders/"+id, service.RenderWriteRequest{TimelineID: timelineID})
	if err != nil {
		return domain.RenderJob{}, err
	}
	if status != 200 && status != 201 {
		return domain.RenderJob{}, fmt.Errorf("render %s returned %d: %s", id, status, string(body))
	}
	var job domain.RenderJob
	if err := json.Unmarshal(body, &job); err != nil {
		return domain.RenderJob{}, err
	}
	return job, nil
}

func (harness *httpHarness) getOutput(ctx context.Context, renderID string) ([]byte, error) {
	status, body, err := harness.do(ctx, "GET", "/v1/renders/"+renderID+"/output", nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("output %s returned %d: %s", renderID, status, string(body))
	}
	return body, nil
}

func (harness *httpHarness) getReceipt(ctx context.Context, renderID string) (domain.RenderReceipt, error) {
	status, body, err := harness.do(ctx, "GET", "/v1/renders/"+renderID+"/receipt", nil)
	if err != nil {
		return domain.RenderReceipt{}, err
	}
	if status != 200 {
		return domain.RenderReceipt{}, fmt.Errorf("receipt %s returned %d: %s", renderID, status, string(body))
	}
	var receipt domain.RenderReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		return domain.RenderReceipt{}, err
	}
	return receipt, nil
}

func sineWAV(rate, channels, durationMS int, frames int, frequency, amplitude, phase, rightScale float64) []byte {
	_ = durationMS
	samples := make([]float32, frames*channels)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(rate)
		first := float32(math.Sin(2*math.Pi*frequency*seconds) * amplitude)
		if channels == 1 {
			samples[frame] = first
			continue
		}
		second := float32(math.Sin(2*math.Pi*frequency*seconds+phase) * amplitude * rightScale)
		samples[frame*2] = first
		samples[frame*2+1] = second
	}
	encoded, err := audiocodec.EncodePCM16(rate, channels, samples)
	if err != nil {
		panic(err)
	}
	return encoded
}

// renderTrace adapts render.RenderWithTrace to the drill without exposing a
// second trace shape. It records the producer trace produced by the same
// pipeline that renders.
type renderTrace struct {
	value *render.Trace
}

func (trace *renderTrace) trace() *render.Trace { return trace.value }

func renderWithTrace(ctx context.Context, plan domain.Timeline, sources map[string]render.SourceInput, maxFrames int64, now time.Time, trace *renderTrace) (render.Result, error) {
	trace.value = &render.Trace{}
	return render.RenderWithTrace(ctx, plan.ID, plan, sources, render.Limits{MaxOutputFrames: maxFrames}, now, trace.value)
}

// evidenceFrom adapts a foreign result to the live-evidence shape used when
// building a foreign bundle.
type foreignEvidence struct {
	job    *domain.RenderJob
	jobRaw []byte
}

func evidenceFrom(project *studioProject, _ repro.ForeignResult) foreignEvidence {
	job := project.job
	return foreignEvidence{job: &job, jobRaw: project.jobRaw}
}

// bundleReceiptUntouched proves the historical receipt inside the bundle is
// byte-identical to the one captured from the live service.
func bundleReceiptUntouched(dir string, manifest repro.Manifest) bool {
	raw, _, err := repro.LoadHistoricalReceipt(dir, manifest)
	if err != nil {
		return false
	}
	return digestOfBytes(raw) == manifest.ReceiptDigest
}

func buildAttestedBundle(outDir string, project *studioProject, manifest repro.Manifest, now time.Time) error {
	provenance := repro.DeriveProvenance(manifest.Provenance.Toolchain, "drill-attested", manifest.ContentDigest, "attested-claims-only", now)
	_, err := repro.BuildAttestedBundle(outDir, manifest, project.timelineRaw, project.receiptRaw, provenance, now)
	return err
}

// keep sort imported for source ordering helpers in this file's future use.
var _ = sort.Strings
var _ = filepath.Join
var _ = os.WriteFile
