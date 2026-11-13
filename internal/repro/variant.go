package repro

// This file is drill-only. It implements a compact second rendition of the
// pipeline whose resampler uses nearest-neighbour lookup instead of linear
// interpolation. The drill labels it as a foreign toolchain with its own
// algorithm digest. It is never reachable from the service wiring.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
)

const VariantAlgorithmVersion = "vtl-pcm16-nearest-resample-v1"

var variantDescriptors = []string{
	"decode:wav-riff-pcm16,signed-little-endian,negative/32768,positive/32767",
	"resample:nearest-neighbour,floor(position),same-target-frame-rounding",
	"channel-map:mono-to-stereo-duplicate,stereo-to-mono-average",
	"gain:linear=pow(10,db/20),applied-before-fade",
	"crossfade:equal-power,sin/cos-of-midpoint-progress,overlap-rewrites-outgoing",
	"normalize:peak-to-linear-target,clamp-only-on-overflow,applied-once",
	"encode:wav-canonical-44-byte-header,pcm16-round-half-away-from-zero",
}

func VariantAlgorithmDigest() string {
	payload := VariantAlgorithmVersion + "\n" + strings.Join(variantDescriptors, "\n") + "\n"
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func VariantToolchain() Toolchain {
	toolchain := CurrentToolchain("voice-timeline-renderer", "drill-variant", "", false)
	toolchain.AlgorithmVersion = VariantAlgorithmVersion
	toolchain.AlgorithmDigest = VariantAlgorithmDigest()
	return toolchain
}

// ForeignResult mirrors render.Result for the variant implementation.
type ForeignResult struct {
	WAV     []byte
	Receipt domain.RenderReceipt
	Output  domain.OutputAsset
	Trace   render.Trace
}

type variantSpan struct {
	index           int
	sourceID        string
	sourceDigest    string
	sourceStart     int64
	sourceEnd       int64
	outputStart     int64
	outputFrames    int64
	crossfadeFrames int64
	gainDB          float64
	action          string
	clip            *audiocodec.Clip
	gainedDigest    string
}

// RenderForeign runs the variant pipeline. It intentionally shares decoding,
// slicing, channel mapping, and encoding with the real codec but replaces the
// resampler and reimplements mixing/normalization in the same shape.
func RenderForeign(renderID string, plan domain.Timeline, sources map[string]render.SourceInput, maxOutputFrames int64, now time.Time) (ForeignResult, error) {
	decoded := make(map[string]*audiocodec.Clip, len(sources))
	spans := make([]variantSpan, 0, len(plan.Spans))
	trace := render.Trace{}
	cursor := int64(0)
	for index, span := range plan.Spans {
		source, ok := sources[span.SourceID]
		if !ok {
			return ForeignResult{}, fmt.Errorf("missing source %q", span.SourceID)
		}
		clip, ok := decoded[span.SourceID]
		if !ok {
			parsed, _, err := audiocodec.DecodeWAV(source.WAV)
			if err != nil {
				return ForeignResult{}, err
			}
			clip = parsed
			decoded[span.SourceID] = parsed
			trace.Sources = append(trace.Sources, render.SourceTrace{
				SourceID: span.SourceID, BytesDigest: digestBytes(source.WAV),
				DecodedFrames: parsed.FrameCount(), SampleRate: parsed.SampleRate,
				Channels: parsed.Channels, PCMDigest: digestSamples(parsed.Samples),
			})
		}
		selected, err := audiocodec.SliceFrames(nil, clip, span.SourceStartFrame, span.SourceEndFrame)
		if err != nil {
			return ForeignResult{}, err
		}
		resampled := nearestResample(selected, plan.SampleRate)
		mapped, action, err := audiocodec.MapChannels(context.Background(), resampled, plan.OutputChannels)
		if err != nil {
			return ForeignResult{}, err
		}
		frames := mapped.FrameCount()
		start := cursor
		if index > 0 {
			start -= span.CrossfadeFrames
		}
		if start < 0 {
			return ForeignResult{}, fmt.Errorf("negative placement")
		}
		cursor = start + frames
		spans = append(spans, variantSpan{
			index: index, sourceID: span.SourceID, sourceDigest: source.Asset.Digest,
			sourceStart: span.SourceStartFrame, sourceEnd: span.SourceEndFrame,
			outputStart: start, outputFrames: frames, crossfadeFrames: span.CrossfadeFrames,
			gainDB: span.GainDB, action: action, clip: mapped,
			gainedDigest: digestGained(mapped.Samples, span.GainDB),
		})
		trace.Spans = append(trace.Spans, render.SpanTrace{
			Index: index, SourceID: span.SourceID,
			SourceStartFrame: span.SourceStartFrame, SourceEndFrame: span.SourceEndFrame,
			TargetSampleRate: plan.SampleRate,
			ResampledFrames: resampled.FrameCount(), ResampledChannels: resampled.Channels,
			ResampledDigest: digestSamples(resampled.Samples), ChannelAction: action,
			MappedFrames: frames, MappedDigest: digestSamples(mapped.Samples),
			OutputStartFrame: start, OutputFrames: frames,
			CrossfadeFrames: span.CrossfadeFrames, GainDB: span.GainDB,
			GainedDigest: digestGained(mapped.Samples, span.GainDB),
		})
	}
	if cursor < 1 {
		return ForeignResult{}, fmt.Errorf("no output frames")
	}
	if maxOutputFrames > 0 && cursor > maxOutputFrames {
		return ForeignResult{}, fmt.Errorf("output exceeds limit")
	}
	mix := make([]float32, int(cursor)*plan.OutputChannels)
	for _, span := range spans {
		gain := float32(audiocodec.DBToLinear(span.gainDB))
		for frame := int64(0); frame < span.outputFrames; frame++ {
			incoming, outgoing := float32(1), float32(1)
			if span.crossfadeFrames > 0 && frame < span.crossfadeFrames {
				progress := (float64(frame) + 0.5) / float64(span.crossfadeFrames)
				angle := progress * math.Pi / 2
				incoming, outgoing = float32(math.Sin(angle)), float32(math.Cos(angle))
			}
			inStart := int(frame) * plan.OutputChannels
			outStart := int(span.outputStart+frame) * plan.OutputChannels
			for channel := 0; channel < plan.OutputChannels; channel++ {
				value := span.clip.Samples[inStart+channel] * gain * incoming
				if span.crossfadeFrames > 0 && frame < span.crossfadeFrames {
					mix[outStart+channel] *= outgoing
				}
				mix[outStart+channel] += value
			}
		}
	}
	metrics := audiocodec.Measure(mix)
	trace.Mix = render.MixTrace{Frames: cursor, Channels: plan.OutputChannels, Digest: digestSamples(mix), PeakDBFS: metrics.PeakDBFS, RMSDBFS: metrics.RMSDBFS}

	before := metrics
	target := audiocodec.DBToLinear(plan.TargetPeakDBFS)
	peak := 0.0
	for _, sample := range mix {
		if value := math.Abs(float64(sample)); value > peak {
			peak = value
		}
	}
	gain := 1.0
	if peak > 0 {
		gain = target / peak
	}
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain <= 0 {
		gain = 1
	}
	for index, sample := range mix {
		value := float64(sample) * gain
		if value > 1 {
			value = 1
		}
		if value < -1 {
			value = -1
		}
		mix[index] = float32(value)
	}
	after := audiocodec.Measure(mix)
	trace.Normalization = render.NormalizationTrace{
		BeforePeakDBFS: before.PeakDBFS, AppliedGainDB: audiocodec.LinearToDB(gain),
		AfterPeakDBFS: after.PeakDBFS, Digest: digestSamples(mix),
	}
	encoded, err := audiocodec.EncodePCM16(plan.SampleRate, plan.OutputChannels, mix)
	if err != nil {
		return ForeignResult{}, err
	}
	output, receipt := foreignReceipt(renderID, plan, encoded, spans, before, gain, now)
	trace.Encoding = render.EncodingTrace{
		SampleRate: output.SampleRate, Channels: output.Channels, BitsPerSample: output.BitsPerSample,
		Frames: output.FrameCount, ByteCount: output.ByteCount, Digest: output.Digest,
	}
	return ForeignResult{WAV: encoded, Receipt: receipt, Output: output, Trace: trace}, nil
}

func nearestResample(clip *audiocodec.Clip, targetRate int) *audiocodec.Clip {
	if clip.SampleRate == targetRate {
		copied := make([]float32, len(clip.Samples))
		copy(copied, clip.Samples)
		return &audiocodec.Clip{SampleRate: targetRate, Channels: clip.Channels, Samples: copied}
	}
	sourceFrames := clip.FrameCount()
	targetFrames := int64(math.Round(float64(sourceFrames) * float64(targetRate) / float64(clip.SampleRate)))
	if targetFrames < 1 {
		targetFrames = 1
	}
	result := make([]float32, int(targetFrames)*clip.Channels)
	step := float64(clip.SampleRate) / float64(targetRate)
	for frame := int64(0); frame < targetFrames; frame++ {
		position := float64(frame) * step
		index := int64(math.Floor(position))
		if index >= sourceFrames {
			index = sourceFrames - 1
		}
		for channel := 0; channel < clip.Channels; channel++ {
			result[int(frame)*clip.Channels+channel] = clip.Samples[int(index)*clip.Channels+channel]
		}
	}
	return &audiocodec.Clip{SampleRate: targetRate, Channels: clip.Channels, Samples: result}
}

func foreignReceipt(renderID string, plan domain.Timeline, encoded []byte, spans []variantSpan, before audiocodec.Metrics, gain float64, now time.Time) (domain.OutputAsset, domain.RenderReceipt) {
	clip, info, err := audiocodec.DecodeWAV(encoded)
	if err != nil {
		panic(err)
	}
	measurements := audiocodec.Measure(clip.Samples)
	output := domain.OutputAsset{
		Digest: digestBytes(encoded), ByteCount: int64(len(encoded)),
		SampleRate: info.SampleRate, Channels: info.Channels, BitsPerSample: info.BitsPerSample,
		FrameCount: info.FrameCount, DurationMS: int64(float64(info.FrameCount) * 1000 / float64(info.SampleRate)),
		PeakDBFS: measurements.PeakDBFS, RMSDBFS: measurements.RMSDBFS,
	}
	receiptSpans := make([]domain.ReceiptSpan, 0, len(spans))
	warnings := make([]string, 0)
	for _, span := range spans {
		receiptSpans = append(receiptSpans, domain.ReceiptSpan{
			Index: span.index, SourceID: span.sourceID, SourceDigest: span.sourceDigest,
			SourceStartFrame: span.sourceStart, SourceEndFrame: span.sourceEnd,
			OutputStartFrame: span.outputStart, OutputEndFrame: span.outputStart + span.outputFrames,
			OutputFrames: span.outputFrames, CrossfadeFrames: span.crossfadeFrames,
			GainDB: span.gainDB, ChannelAction: span.action,
		})
		if span.action != "unchanged" {
			warnings = append(warnings, fmt.Sprintf("span %d applied %s", span.index, span.action))
		}
	}
	receipt := domain.RenderReceipt{
		SchemaVersion: 1, RenderID: renderID, TimelineID: plan.ID, Output: output,
		Normalization: domain.NormalizationReceipt{
			RequestedPeakDBFS: plan.TargetPeakDBFS, BeforePeakDBFS: before.PeakDBFS,
			AppliedGainDB: audiocodec.LinearToDB(gain), OutputPeakDBFS: measurements.PeakDBFS,
		},
		Spans: receiptSpans, Warnings: warnings, CreatedAt: now.UTC(),
	}
	return output, receipt
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func digestSamples(samples []float32) string {
	hash := sha256.New()
	var word [4]byte
	for _, sample := range samples {
		binary.LittleEndian.PutUint32(word[:], math.Float32bits(sample))
		_, _ = hash.Write(word[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func digestGained(samples []float32, gainDB float64) string {
	gain := float32(math.Pow(10, gainDB/20))
	hash := sha256.New()
	var word [4]byte
	for _, sample := range samples {
		binary.LittleEndian.PutUint32(word[:], math.Float32bits(sample*gain))
		_, _ = hash.Write(word[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}
