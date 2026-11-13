package repro

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"voice-timeline-renderer/internal/domain"
)

// SchemaVersion is bumped whenever the bundle layout or manifest semantics
// change in a way that older readers cannot interpret safely.
const SchemaVersion = 1

// Evidence levels describe how far a third party can go with a bundle.
const (
	// LevelReplayable means every referenced source WAV, the timeline, the
	// historical receipt, and the toolchain fingerprint are present, so the
	// render can be re-executed and compared layer by layer.
	LevelReplayable = "replayable"
	// LevelAttested means the bundle carries digests and receipts but not the
	// exact source bytes, so a third party can only re-check internal
	// consistency, not produce a new mixdown.
	LevelAttested = "attested"
)

// FileRecord is one integrity entry in the bundle file list.
type FileRecord struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// SourceRecord describes one bundled source asset.
type SourceRecord struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Digest        string `json:"digest"`
	ByteCount     int64  `json:"byte_count"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bits_per_sample"`
	FrameCount    int64  `json:"frame_count"`
	DurationMS    int64  `json:"duration_ms"`
	PeakDBFS      float64 `json:"peak_dbfs"`
	RMSDBFS       float64 `json:"rms_dbfs"`
	MediaFile     string `json:"media_file"`
	MetaFile      string `json:"metadata_file"`
}

// Manifest is the self-describing index of one reproduction evidence bundle.
type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	Project       string         `json:"project"`
	RenderID      string         `json:"render_id"`
	TimelineID    string         `json:"timeline_id"`
	EvidenceLevel string         `json:"evidence_level"`
	Sources       []SourceRecord `json:"sources"`
	TimelineFile  string         `json:"timeline_file"`
	TimelineDigest string        `json:"timeline_digest"`
	ReceiptFile   string         `json:"receipt_file"`
	ReceiptDigest string         `json:"receipt_digest"`
	OutputFile    string         `json:"output_file,omitempty"`
	OutputDigest  string         `json:"output_digest,omitempty"`
	JobFile       string         `json:"job_file,omitempty"`
	JobDigest     string         `json:"job_digest,omitempty"`
	TraceFile     string         `json:"trace_file,omitempty"`
	TraceDigest   string         `json:"trace_digest,omitempty"`
	Files         []FileRecord   `json:"files"`
	Provenance    Provenance     `json:"provenance"`
	CapturedAt    time.Time      `json:"captured_at"`
	ContentDigest string         `json:"content_digest"`
}

// BundleOptions is the minimum set of evidence a producer must hand over for
// a replayable bundle. The historical receipt is stored verbatim; it is never
// rewritten by a later verification.
type BundleOptions struct {
	Project    string
	Timeline   domain.Timeline
	TimelineRaw []byte
	Sources    map[string]BundledSource
	Receipt    domain.RenderReceipt
	ReceiptRaw []byte
	OutputWAV  []byte
	Job        *domain.RenderJob
	JobRaw     []byte
	Trace      *TraceEnvelope
	Provenance Provenance
	Now        time.Time
}

// BundledSource is one source asset with the exact media bytes.
type BundledSource struct {
	Asset    domain.SourceAsset
	MetadataRaw []byte
	WAV      []byte
}

// TraceEnvelope wraps an optional stage trace with its origin so a reader can
// tell whether it was recorded by the producer or reconstructed by a verifier.
type TraceEnvelope struct {
	Origin string `json:"origin"`
	Toolchain Toolchain `json:"toolchain"`
	Trace  any    `json:"trace"`
}

// BuildBundle writes a bundle directory. It stages the content in a sibling
// temporary directory and renames it into place, so a partial bundle is never
// visible, and it refuses to overwrite an existing bundle.
func BuildBundle(outDir string, options BundleOptions) (Manifest, error) {
	if outDir == "" {
		return Manifest{}, errors.New("bundle output directory is required")
	}
	if options.Receipt.SchemaVersion == 0 && len(options.ReceiptRaw) == 0 {
		return Manifest{}, errors.New("historical receipt is required to build a bundle")
	}
	if len(options.TimelineRaw) == 0 {
		return Manifest{}, errors.New("timeline evidence is required to build a bundle")
	}
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	if _, err := os.Stat(outDir); err == nil {
		return Manifest{}, fmt.Errorf("bundle %q already exists; bundles are immutable", outDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	parent := filepath.Dir(outDir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return Manifest{}, err
	}
	temp, err := os.MkdirTemp(parent, ".bundle-*")
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		_ = os.RemoveAll(temp)
	}()

	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Project:       options.Project,
		RenderID:      renderIDFrom(options),
		TimelineID:    timelineIDFrom(options),
		Provenance:    options.Provenance,
		CapturedAt:    options.Now.UTC(),
		TimelineFile:  "timeline.json",
		ReceiptFile:   "receipt.json",
	}
	if err := writeBundleFile(temp, manifest.TimelineFile, options.TimelineRaw); err != nil {
		return Manifest{}, err
	}
	manifest.TimelineDigest = digestOf(options.TimelineRaw)
	if err := writeBundleFile(temp, manifest.ReceiptFile, options.ReceiptRaw); err != nil {
		return Manifest{}, err
	}
	manifest.ReceiptDigest = digestOf(options.ReceiptRaw)
	if len(options.OutputWAV) > 0 {
		manifest.OutputFile = "output.wav"
		if err := writeBundleFile(temp, manifest.OutputFile, options.OutputWAV); err != nil {
			return Manifest{}, err
		}
		manifest.OutputDigest = digestOf(options.OutputWAV)
	}
	if options.Job != nil && len(options.JobRaw) > 0 {
		manifest.JobFile = "job.json"
		if err := writeBundleFile(temp, manifest.JobFile, options.JobRaw); err != nil {
			return Manifest{}, err
		}
		manifest.JobDigest = digestOf(options.JobRaw)
	}
	if options.Trace != nil {
		raw, err := json.Marshal(options.Trace)
		if err != nil {
			return Manifest{}, err
		}
		manifest.TraceFile = "trace.json"
		if err := writeBundleFile(temp, manifest.TraceFile, raw); err != nil {
			return Manifest{}, err
		}
		manifest.TraceDigest = digestOf(raw)
	}

	ids := make([]string, 0, len(options.Sources))
	for id := range options.Sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	level := LevelReplayable
	for _, id := range ids {
		source := options.Sources[id]
		record := SourceRecord{
			ID:            source.Asset.ID,
			Name:          source.Asset.Name,
			Digest:        source.Asset.Digest,
			ByteCount:     source.Asset.ByteCount,
			SampleRate:    source.Asset.SampleRate,
			Channels:      source.Asset.Channels,
			BitsPerSample: source.Asset.BitsPerSample,
			FrameCount:    source.Asset.FrameCount,
			DurationMS:    source.Asset.DurationMS,
			PeakDBFS:      source.Asset.PeakDBFS,
			RMSDBFS:       source.Asset.RMSDBFS,
			MediaFile:     filepath.ToSlash(filepath.Join("sources", id, "source.wav")),
			MetaFile:      filepath.ToSlash(filepath.Join("sources", id, "metadata.json")),
		}
		if len(source.WAV) == 0 {
			return Manifest{}, fmt.Errorf("source %q has no media bytes; a replayable bundle needs exact bytes", id)
		}
		if digestOf(source.WAV) != source.Asset.Digest {
			return Manifest{}, fmt.Errorf("source %q media bytes do not match the recorded digest", id)
		}
		if err := writeBundleFile(temp, record.MediaFile, source.WAV); err != nil {
			return Manifest{}, err
		}
		metadata := source.MetadataRaw
		if len(metadata) == 0 {
			encoded, err := json.MarshalIndent(source.Asset, "", "  ")
			if err != nil {
				return Manifest{}, err
			}
			metadata = append(encoded, '\n')
		}
		if err := writeBundleFile(temp, record.MetaFile, metadata); err != nil {
			return Manifest{}, err
		}
		manifest.Sources = append(manifest.Sources, record)
	}
	if len(manifest.Sources) == 0 {
		return Manifest{}, errors.New("a replayable bundle needs at least one source")
	}
	manifest.EvidenceLevel = level

	fileRecords, err := collectFiles(temp, "manifest.json")
	if err != nil {
		return Manifest{}, err
	}
	manifest.Files = fileRecords
	canonical, err := canonicalManifest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.ContentDigest = digestOf(canonical)
	final, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := writeBundleFile(temp, "manifest.json", append(final, '\n')); err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(temp, outDir); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func renderIDFrom(options BundleOptions) string {
	if options.Receipt.RenderID != "" {
		return options.Receipt.RenderID
	}
	var receipt domain.RenderReceipt
	if err := json.Unmarshal(options.ReceiptRaw, &receipt); err == nil {
		return receipt.RenderID
	}
	return ""
}

func timelineIDFrom(options BundleOptions) string {
	if options.Receipt.TimelineID != "" {
		return options.Receipt.TimelineID
	}
	if options.Timeline.ID != "" {
		return options.Timeline.ID
	}
	var receipt domain.RenderReceipt
	if err := json.Unmarshal(options.ReceiptRaw, &receipt); err == nil {
		return receipt.TimelineID
	}
	return ""
}

func canonicalManifest(manifest Manifest) ([]byte, error) {
	manifest.ContentDigest = ""
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func collectFiles(root, skip string) ([]FileRecord, error) {
	records := make([]FileRecord, 0)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == skip {
			return nil
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		records = append(records, FileRecord{
			Path:   relative,
			Size:   int64(len(payload)),
			Digest: digestOf(payload),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return records, nil
}

func writeBundleFile(root, relative string, payload []byte) error {
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, payload, 0o640)
}

func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// LoadBundle reads and integrity-checks a bundle directory. Every file record
// is verified against the manifest before any evidence is trusted.
func LoadBundle(dir string) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion {
		return Manifest{}, fmt.Errorf("unsupported bundle schema version %d", manifest.SchemaVersion)
	}
	stored := manifest.ContentDigest
	manifest.ContentDigest = ""
	canonical, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	canonical = append(canonical, '\n')
	if digestOf(canonical) != stored {
		return Manifest{}, errors.New("bundle manifest digest does not match its content")
	}
	manifest.ContentDigest = stored
	for _, record := range manifest.Files {
		payload, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(record.Path)))
		if err != nil {
			return Manifest{}, fmt.Errorf("bundle file %q is missing: %w", record.Path, err)
		}
		if int64(len(payload)) != record.Size || digestOf(payload) != record.Digest {
			return Manifest{}, fmt.Errorf("bundle file %q does not match the manifest", record.Path)
		}
	}
	return manifest, nil
}
