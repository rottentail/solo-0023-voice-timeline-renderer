package httpapi

import (
	"net/http"

	"voice-timeline-renderer/internal/service"
)

func (server *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) handleSourceWrite(w http.ResponseWriter, request *http.Request) {
	var payload service.SourceWriteRequest
	if err := decodeJSON(w, request, &payload, server.options.MaxSourceRequestBytes); err != nil {
		writeError(w, err, nil)
		return
	}
	asset, created, err := server.service.CreateSource(request.Context(), request.PathValue("id"), payload)
	if err != nil {
		writeError(w, err, nil)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, asset)
}

func (server *Server) handleSourceRead(w http.ResponseWriter, request *http.Request) {
	asset, err := server.service.GetSource(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, asset)
}
