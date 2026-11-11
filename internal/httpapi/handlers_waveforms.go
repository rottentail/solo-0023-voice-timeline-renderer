package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
)

func (server *Server) handleWaveformRead(w http.ResponseWriter, request *http.Request) {
	query, err := parseWaveformQuery(request.URL.Query())
	if err != nil {
		writeError(w, err, nil)
		return
	}
	summary, err := server.service.GetWaveform(request.Context(), request.PathValue("id"), query)
	if err != nil {
		writeError(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func parseWaveformQuery(values url.Values) (service.WaveformQuery, error) {
	var query service.WaveformQuery

	start, err := optionalInt64Query(values, "start_frame", "start", "from_frame")
	if err != nil {
		return query, err
	}
	query.StartFrame = start

	end, err := optionalInt64Query(values, "end_frame", "end", "to_frame")
	if err != nil {
		return query, err
	}
	query.EndFrame = end

	sampleRate, err := optionalInt64Query(values, "sample_rate", "target_sample_rate")
	if err != nil {
		return query, err
	}
	if sampleRate != nil {
		value := int(*sampleRate)
		query.SampleRate = &value
	}

	buckets, err := optionalInt64Query(values, "buckets", "bucket_count")
	if err != nil {
		return query, err
	}
	query.BucketCount = buckets

	return query, nil
}

func optionalInt64Query(values url.Values, names ...string) (*int64, error) {
	for _, name := range names {
		rawValues, present := values[name]
		if !present {
			continue
		}
		if len(rawValues) == 0 || rawValues[0] == "" {
			return nil, domain.E(
				domain.KindInvalid,
				"invalid_query_parameter",
				name,
				"query parameter must contain an integer",
			)
		}
		parsed, err := strconv.ParseInt(rawValues[0], 10, 64)
		if err != nil {
			return nil, domain.E(
				domain.KindInvalid,
				"invalid_query_parameter",
				name,
				"query parameter must contain an integer",
			)
		}
		return &parsed, nil
	}
	return nil, nil
}
