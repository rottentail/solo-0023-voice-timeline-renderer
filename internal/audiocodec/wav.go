package audiocodec

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	pcmFormatCode = 1
	pcmBitWidth   = 16
)

func DecodeWAV(data []byte) (*Clip, WAVInfo, error) {
	if len(data) < 12 {
		return nil, WAVInfo{}, errors.New("file is shorter than a RIFF header")
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, WAVInfo{}, errors.New("file is not a RIFF/WAVE payload")
	}

	riffSize := uint64(binary.LittleEndian.Uint32(data[4:8]))
	if riffSize+8 > uint64(len(data)) {
		return nil, WAVInfo{}, errors.New("RIFF size exceeds the supplied payload")
	}

	var formatChunk []byte
	var sampleChunk []byte
	offset := 12
	for offset+8 <= len(data) {
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		payloadStart := offset + 8
		payloadEnd := payloadStart + chunkSize
		if chunkSize < 0 || payloadEnd < payloadStart || payloadEnd > len(data) {
			return nil, WAVInfo{}, fmt.Errorf("chunk %q exceeds the payload", chunkID)
		}
		switch chunkID {
		case "fmt ":
			formatChunk = data[payloadStart:payloadEnd]
		case "data":
			sampleChunk = data[payloadStart:payloadEnd]
		}
		offset = payloadEnd
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if len(formatChunk) < 16 {
		return nil, WAVInfo{}, errors.New("fmt chunk is missing or too short")
	}
	if sampleChunk == nil {
		return nil, WAVInfo{}, errors.New("data chunk is missing")
	}

	audioFormat := binary.LittleEndian.Uint16(formatChunk[0:2])
	channels := int(binary.LittleEndian.Uint16(formatChunk[2:4]))
	sampleRate := int(binary.LittleEndian.Uint32(formatChunk[4:8]))
	byteRate := binary.LittleEndian.Uint32(formatChunk[8:12])
	blockAlign := int(binary.LittleEndian.Uint16(formatChunk[12:14]))
	bitsPerSample := int(binary.LittleEndian.Uint16(formatChunk[14:16]))

	if audioFormat != pcmFormatCode {
		return nil, WAVInfo{}, fmt.Errorf("unsupported WAV format code %d", audioFormat)
	}
	if bitsPerSample != pcmBitWidth {
		return nil, WAVInfo{}, fmt.Errorf("unsupported PCM width %d", bitsPerSample)
	}
	if channels != 1 && channels != 2 {
		return nil, WAVInfo{}, fmt.Errorf("unsupported channel count %d", channels)
	}
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, WAVInfo{}, fmt.Errorf("sample rate %d is outside the accepted range", sampleRate)
	}
	expectedBlockAlign := channels * (bitsPerSample / 8)
	if blockAlign != expectedBlockAlign {
		return nil, WAVInfo{}, errors.New("block alignment does not match channels and sample width")
	}
	expectedByteRate := uint32(sampleRate * blockAlign)
	if byteRate != expectedByteRate {
		return nil, WAVInfo{}, errors.New("byte rate does not match sample rate and block alignment")
	}
	if len(sampleChunk) == 0 || len(sampleChunk)%blockAlign != 0 {
		return nil, WAVInfo{}, errors.New("sample data is empty or not frame-aligned")
	}

	samples := make([]float32, len(sampleChunk)/2)
	for index := range samples {
		raw := int16(binary.LittleEndian.Uint16(sampleChunk[index*2 : index*2+2]))
		if raw < 0 {
			samples[index] = float32(raw) / 32768
		} else {
			samples[index] = float32(raw) / 32767
		}
	}
	frameCount := int64(len(samples) / channels)
	return &Clip{
		SampleRate: sampleRate,
		Channels:   channels,
		Samples:    samples,
	}, WAVInfo{
		SampleRate:    sampleRate,
		Channels:      channels,
		BitsPerSample: bitsPerSample,
		FrameCount:    frameCount,
		ByteCount:     int64(len(data)),
	}, nil
}
