package render

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
)

// Trace is an optional, read-only record of the intermediate values produced
// by one render execution. It exists only for reproduction diagnostics: when a
// caller passes nil, the render pipeline runs exactly as before with no hashing
// or allocation overhead.
type Trace struct {
	Sources       []SourceTrace      `json:"sources"`
	Spans         []SpanTrace        `json:"spans"`
	Mix           MixTrace           `json:"mix"`
	Normalization NormalizationTrace `json:"normalization"`
	Encoding      EncodingTrace      `json:"encoding"`
}

type SourceTrace struct {
	SourceID      string `json:"source_id"`
	BytesDigest   string `json:"bytes_digest"`
	DecodedFrames int64  `json:"decoded_frames"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	PCMDigest     string `json:"pcm_digest"`
}

type SpanTrace struct {
	Index            int     `json:"index"`
	SourceID         string  `json:"source_id"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	TargetSampleRate int     `json:"target_sample_rate"`
	ResampledFrames  int64   `json:"resampled_frames"`
	ResampledChannels int    `json:"resampled_channels"`
	ResampledDigest  string  `json:"resampled_digest"`
	ChannelAction    string  `json:"channel_action"`
	MappedFrames     int64   `json:"mapped_frames"`
	MappedDigest     string  `json:"mapped_digest"`
	OutputStartFrame int64   `json:"output_start_frame"`
	OutputFrames     int64   `json:"output_frames"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
	GainDB           float64 `json:"gain_db"`
	// GainedDigest is the selected clip after gain, before fade weighting and
	// placement. It isolates the gain layer from the crossfade/mix layer.
	GainedDigest string `json:"gained_digest"`
}

type MixTrace struct {
	Frames   int64   `json:"frames"`
	Channels int     `json:"channels"`
	Digest   string  `json:"digest"`
	PeakDBFS float64 `json:"peak_dbfs"`
	RMSDBFS  float64 `json:"rms_dbfs"`
}

type NormalizationTrace struct {
	BeforePeakDBFS float64 `json:"before_peak_dbfs"`
	AppliedGainDB  float64 `json:"applied_gain_db"`
	AfterPeakDBFS  float64 `json:"after_peak_dbfs"`
	Digest         string  `json:"digest"`
}

type EncodingTrace struct {
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bits_per_sample"`
	Frames        int64  `json:"frames"`
	ByteCount     int64  `json:"byte_count"`
	Digest        string `json:"digest"`
}

func bytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func pcmDigest(samples []float32) string {
	hash := sha256.New()
	var word [4]byte
	for _, sample := range samples {
		binary.LittleEndian.PutUint32(word[:], math.Float32bits(sample))
		_, _ = hash.Write(word[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// gainedDigest mirrors the mixer's gain arithmetic exactly: the same float32
// multiplication, in the same order, minus the fade weight.
func gainedDigest(samples []float32, gainDB float64) string {
	gain := float32(math.Pow(10, gainDB/20))
	hash := sha256.New()
	var word [4]byte
	for _, sample := range samples {
		binary.LittleEndian.PutUint32(word[:], math.Float32bits(sample*gain))
		_, _ = hash.Write(word[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}
