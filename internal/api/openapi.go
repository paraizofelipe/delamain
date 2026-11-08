package api

import (
	_ "embed"
	"net/http"
)

// openAPI is the OpenAPI 3.1 description of this API, the contract an app generates its client
// from; GET /api/v1/openapi.yaml serves it behind the token.
//
//go:embed openapi.yaml
var openAPI []byte

// OpenAPI is the embedded document.
func OpenAPI() []byte { return openAPI }

func (s *Server) docRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(openAPI)
	})
}
