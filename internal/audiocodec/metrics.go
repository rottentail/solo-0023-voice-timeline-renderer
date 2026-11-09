package audiocodec

import "math"

const silenceFloorDBFS = -120.0

func Measure(samples []float32) Metrics {
	if len(samples) == 0 {
		return Metrics{PeakDBFS: silenceFloorDBFS, RMSDBFS: silenceFloorDBFS}
	}
	peak := 0.0
	sumSquares := 0.0
	for _, sample := range samples {
		value := float64(sample)
		absolute := math.Abs(value)
		if absolute > peak {
			peak = absolute
		}
		sumSquares += value * value
	}
	rms := math.Sqrt(sumSquares / float64(len(samples)))
	return Metrics{
		PeakDBFS: amplitudeDBFS(peak),
		RMSDBFS:  amplitudeDBFS(rms),
	}
}

func amplitudeDBFS(value float64) float64 {
	if value <= 0 {
		return silenceFloorDBFS
	}
	measured := 20 * math.Log10(value)
	if measured < silenceFloorDBFS {
		return silenceFloorDBFS
	}
	return measured
}

func DBToLinear(db float64) float64 {
	return math.Pow(10, db/20)
}

func LinearToDB(linear float64) float64 {
	return amplitudeDBFS(linear)
}
