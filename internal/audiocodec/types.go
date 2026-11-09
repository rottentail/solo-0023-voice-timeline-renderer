package audiocodec

type Clip struct {
	SampleRate int
	Channels   int
	Samples    []float32
}

func (clip *Clip) FrameCount() int64 {
	if clip == nil || clip.Channels <= 0 {
		return 0
	}
	return int64(len(clip.Samples) / clip.Channels)
}

type WAVInfo struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	FrameCount    int64
	ByteCount     int64
}

type Metrics struct {
	PeakDBFS float64
	RMSDBFS  float64
}
