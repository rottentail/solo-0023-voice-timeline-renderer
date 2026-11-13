package repro

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// AlgorithmVersion names the exact transform contract that produced a render.
// Any change to resampling, channel mapping, crossfade weighting, gain
// conversion, normalization, WAV layout, or PCM quantization must bump this
// string. It is the machine-independent half of the toolchain fingerprint.
const AlgorithmVersion = "vtl-pcm16-linear-v1"

// algorithmDescriptors documents every transform whose implementation detail
// can change output samples. They are hashed into AlgorithmDigest so two
// machines can agree on "same processing chain" without shipping binaries.
var algorithmDescriptors = []string{
	"decode:wav-riff-pcm16,signed-little-endian,negative/32768,positive/32767",
	"decode:accepts-extra-chunks,rof-size-checked,channels-1-or-2",
	"resample:linear-interpolation,round(source*frames*target/source-rate),clamp-last-frame",
	"channel-map:mono-to-stereo-duplicate,stereo-to-mono-average",
	"gain:linear=pow(10,db/20),applied-before-fade",
	"crossfade:equal-power,sin/cos-of-midpoint-progress,overlap-rewrites-outgoing",
	"normalize:peak-to-linear-target,clamp-only-on-overflow,applied-once",
	"measure:peak-and-rms-over-all-samples,floor=-120dBFS",
	"encode:wav-canonical-44-byte-header,pcm16-round-half-away-from-zero",
	"timeline:cursor=sum(resampled-frames)-crossfade,span-starts-at-cursor-minus-crossfade",
}

// AlgorithmDigest is a stable digest of the transform contract. It is
// identical on every machine that runs the same AlgorithmVersion and
// descriptors, and it is what makes byte comparison admissible.
func AlgorithmDigest() string {
	payload := AlgorithmVersion + "\n" + strings.Join(algorithmDescriptors, "\n") + "\n"
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// Toolchain fingerprints the runtime and transform contract. GoVersion and
// GOARCH are declared for transparency but are NOT treated as byte-comparable
// evidence on their own: a rebuilt binary can differ without a version change.
type Toolchain struct {
	GoVersion       string `json:"go_version"`
	GOOS            string `json:"goos"`
	GOARCH          string `json:"goarch"`
	Module          string `json:"module"`
	ModuleVersion   string `json:"module_version,omitempty"`
	VCSRevision     string `json:"vcs_revision,omitempty"`
	VCSModified     bool   `json:"vcs_modified,omitempty"`
	AlgorithmVersion string `json:"algorithm_version"`
	AlgorithmDigest string `json:"algorithm_digest"`
}

// CurrentToolchain captures the fingerprint of this process.
func CurrentToolchain(module, moduleVersion, revision string, modified bool) Toolchain {
	return Toolchain{
		GoVersion:        runtime.Version(),
		GOOS:             runtime.GOOS,
		GOARCH:           runtime.GOARCH,
		Module:           module,
		ModuleVersion:    moduleVersion,
		VCSRevision:      revision,
		VCSModified:      modified,
		AlgorithmVersion: AlgorithmVersion,
		AlgorithmDigest:  AlgorithmDigest(),
	}
}

// Provenance says where evidence came from. It deliberately records lineage
// instead of forcing a third party to copy an entire run directory: the bundle
// is self-contained, and a re-created artifact points back at the historical
// bundle it was derived from.
type Provenance struct {
	Producer       string    `json:"producer"`
	CapturedBy     string    `json:"captured_by"`
	SourceOfTruth  string    `json:"source_of_truth"`
	Toolchain      Toolchain `json:"toolchain"`
	Lineage        []string  `json:"lineage,omitempty"`
	Notes          []string  `json:"notes,omitempty"`
	CapturedAt     time.Time `json:"captured_at"`
}

// ServiceConfig is the declared runtime configuration that bounds a render.
// It is recorded because limits alone can turn an otherwise reproducible
// render into a rejected one.
type ServiceConfig struct {
	MaxSourceBytes  int64 `json:"max_source_bytes"`
	MaxSpans        int   `json:"max_spans"`
	MaxOutputFrames int64 `json:"max_output_frames"`
}

// ProvenanceForRender builds provenance for evidence captured by a live
// service render.
func ProvenanceForRender(toolchain Toolchain, config ServiceConfig, capturedBy string, now time.Time) Provenance {
	return Provenance{
		Producer:      "voice-timeline-renderer",
		CapturedBy:    capturedBy,
		SourceOfTruth: "live-render",
		Toolchain:     toolchain,
		Notes: []string{
			fmt.Sprintf("service limits: source<=%d bytes, spans<=%d, output<=%d frames", config.MaxSourceBytes, config.MaxSpans, config.MaxOutputFrames),
		},
		CapturedAt: now.UTC(),
	}
}

// DeriveProvenance builds provenance for a re-created artifact, keeping an
// explicit lineage pointer to the historical bundle.
func DeriveProvenance(toolchain Toolchain, capturedBy, parentDigest, basis string, now time.Time) Provenance {
	lineage := make([]string, 0, 1)
	if parentDigest != "" {
		lineage = append(lineage, "parent-bundle:"+parentDigest)
	}
	return Provenance{
		Producer:      "voice-timeline-renderer",
		CapturedBy:    capturedBy,
		SourceOfTruth: basis,
		Toolchain:     toolchain,
		Lineage:       lineage,
		CapturedAt:    now.UTC(),
	}
}

func encodeProvenance(provenance Provenance) ([]byte, error) {
	encoded, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
