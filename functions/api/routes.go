// Package api provides the HTTP handler layer for the Functions Management REST API.
//
// This file (routes.go) handles HTTP route registration for the Functions
// management REST API using chi/v5 router (Sprint 4-6, Epic E-018).
//
// It exports two functions:
//   - Routes(h *Handler) chi.Router — primary route wiring, accepts a pre-built Handler
//   - NewRouter(...) chi.Router — convenience constructor that combines handler creation
//     and route registration
//
// The returned chi.Router is designed to be mounted by the Gateway's HTTP server
// at a path like "/v1/functions" via srvMux.Mount(), following the exact pattern
// used by services/rsources/http/http.go (NewV1Handler, NewV2Handler) and
// internal/drain-config/http.go (DrainConfigHttpHandler).
//
// Route registration pattern reference:
//   - services/rsources/http/http.go lines 23-33: chi.NewRouter(), register routes, return
//   - internal/drain-config/http.go lines 10-14: same create-register-return pattern
//   - gateway/handle_lifecycle.go lines 592-601: Mount("/v1/job-status", handler)
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/rudderlabs/rudder-go-kit/logger"
)

// Routes returns a chi.Router with all Functions management REST API endpoints
// registered. The router is designed to be mounted by the Gateway's HTTP server
// at a path like "/v1/functions" via srvMux.Mount().
//
// Registered endpoints:
//
//	POST   /           - Create a new function
//	GET    /           - List functions (with workspaceId query param)
//	GET    /{id}       - Get a function by ID
//	PUT    /{id}       - Update a function by ID
//	DELETE /{id}       - Delete a function by ID
//	POST   /{id}/test  - Test invoke a function with sample payload
//
// The route pattern follows the existing REST API patterns in the repository:
//   - services/rsources/http/http.go (NewV1Handler, NewV2Handler)
//   - warehouse/healthmonitor/handler.go (route mounting)
//   - internal/drain-config/http.go (DrainConfigHttpHandler)
//
// Usage from Gateway (gateway/handle_lifecycle.go):
//
//	functionsHandler := api.NewHandler(log, repo, runtime, secrets)
//	srvMux.Mount("/v1/functions", api.Routes(functionsHandler))
func Routes(h *Handler) chi.Router {
	r := chi.NewRouter()

	// Apply JSON content-type middleware to all routes in this group.
	// This follows the withContentType pattern from gateway/handle_http.go
	// lines 147-152, applied as chi middleware so every response in this
	// sub-router automatically gets the correct Content-Type header.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			next.ServeHTTP(w, req)
		})
	})

	// Register CRUD routes. The {id} URL parameter is extracted in handlers
	// via chi.URLParam(r, "id"), following the pattern from
	// services/rsources/http/http.go line 29 and line 252.
	//
	// Routes are registered WITHOUT the "/v1/functions" prefix — that prefix
	// is applied when the Gateway mounts this router via srvMux.Mount().
	r.Post("/", h.createFunction)       // POST /v1/functions — Create
	r.Get("/", h.listFunctions)         // GET /v1/functions — List
	r.Get("/{id}", h.getFunction)       // GET /v1/functions/{id} — Read
	r.Put("/{id}", h.updateFunction)    // PUT /v1/functions/{id} — Update
	r.Delete("/{id}", h.deleteFunction) // DELETE /v1/functions/{id} — Delete
	r.Post("/{id}/test", h.testFunction) // POST /v1/functions/{id}/test — Test invoke

	// Secrets management (E-019)
	r.Put("/{id}/secrets", h.setSecret)          // PUT /v1/functions/{id}/secrets — Set secret
	r.Get("/{id}/secrets", h.getAllSecrets)       // GET /v1/functions/{id}/secrets — List all secrets
	r.Get("/{id}/secrets/{key}", h.getSecret)     // GET /v1/functions/{id}/secrets/{key} — Get secret
	r.Delete("/{id}/secrets/{key}", h.deleteSecret) // DELETE /v1/functions/{id}/secrets/{key} — Delete secret

	return r
}

// NewRouter creates a new Functions API router with the given dependencies.
// This is a convenience constructor that combines NewHandler and Routes,
// following the NewV1Handler pattern from services/rsources/http/http.go
// lines 23-33 which creates the handler and builds the router in one step.
//
// Usage:
//
//	router := api.NewRouter(log, repo, runtime, secrets)
//	srvMux.Mount("/v1/functions", router)
func NewRouter(log logger.Logger, repo FunctionRepository, rt FunctionRuntime, secrets SecretsManager) chi.Router {
	h := NewHandler(log, repo, rt, secrets)
	return Routes(h)
}
