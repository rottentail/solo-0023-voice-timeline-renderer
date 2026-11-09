package httpapi

import (
	"fmt"
	"net/http"

	"voice-timeline-renderer/internal/service"
)

func (server *Server) handleRenderWrite(w http.ResponseWriter, request *http.Request) {
	var payload service.RenderWriteRequest
	if err := decodeJSON(w, request, &payload, server.options.MaxJSONBytes); err != nil {
		writeError(w, err, nil)
		return
	}
	job, created, err := server.service.CreateRender(request.Context(), request.PathValue("id"), payload)
	if err != nil {
		writeError(w, err, &job)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, job)
}

func (server *Server) handleRenderRead(w http.ResponseWriter, request *http.Request) {
	job, err := server.service.GetRender(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (server *Server) handleReceiptRead(w http.ResponseWriter, request *http.Request) {
	receipt, err := server.service.GetReceipt(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (server *Server) handleOutputRead(w http.ResponseWriter, request *http.Request) {
	payload, receipt, err := server.service.GetOutput(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(w, err, nil)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", receipt.RenderID+".wav"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}
