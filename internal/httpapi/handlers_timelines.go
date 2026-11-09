package httpapi

import (
	"net/http"

	"voice-timeline-renderer/internal/service"
)

func (server *Server) handleTimelineWrite(w http.ResponseWriter, request *http.Request) {
	var payload service.TimelineWriteRequest
	if err := decodeJSON(w, request, &payload, server.options.MaxJSONBytes); err != nil {
		writeError(w, err, nil)
		return
	}
	plan, created, err := server.service.CreateTimeline(request.Context(), request.PathValue("id"), payload)
	if err != nil {
		writeError(w, err, nil)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, plan)
}

func (server *Server) handleTimelineRead(w http.ResponseWriter, request *http.Request) {
	plan, err := server.service.GetTimeline(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}
