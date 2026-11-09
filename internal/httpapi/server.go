package httpapi

import (
	"net/http"

	"voice-timeline-renderer/internal/service"
)

type Options struct {
	MaxSourceRequestBytes int64
	MaxJSONBytes          int64
}

type Server struct {
	service *service.Service
	options Options
	router  *http.ServeMux
}

func New(serviceLayer *service.Service, options Options) *Server {
	if options.MaxSourceRequestBytes <= 0 {
		options.MaxSourceRequestBytes = 48 << 20
	}
	if options.MaxJSONBytes <= 0 {
		options.MaxJSONBytes = 2 << 20
	}
	server := &Server{
		service: serviceLayer,
		options: options,
		router:  http.NewServeMux(),
	}
	server.routes()
	return server
}

func (server *Server) Handler() http.Handler {
	return withRecovery(withHeaders(server.router))
}

func (server *Server) routes() {
	server.router.HandleFunc("GET /healthz", server.handleHealth)
	server.router.HandleFunc("PUT /v1/sources/{id}", server.handleSourceWrite)
	server.router.HandleFunc("GET /v1/sources/{id}", server.handleSourceRead)
	server.router.HandleFunc("PUT /v1/timelines/{id}", server.handleTimelineWrite)
	server.router.HandleFunc("GET /v1/timelines/{id}", server.handleTimelineRead)
	server.router.HandleFunc("PUT /v1/renders/{id}", server.handleRenderWrite)
	server.router.HandleFunc("GET /v1/renders/{id}", server.handleRenderRead)
	server.router.HandleFunc("GET /v1/renders/{id}/receipt", server.handleReceiptRead)
	server.router.HandleFunc("GET /v1/renders/{id}/output", server.handleOutputRead)
}
