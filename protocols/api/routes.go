// Package api provides HTTP route registration for the Tracking Plan Management REST API (E-024).
//
// This file defines the route table and wires each route pattern to the corresponding
// Handler method. It follows the exact pattern from services/rsources/http/http.go
// which creates a chi.Router, registers route handlers, and returns http.Handler.
//
// Routes are relative to the mount point. When the gateway mounts this router
// under "/v1/protocols", the full paths become:
//
//	POST   /v1/protocols/tracking-plans              — Create a new tracking plan
//	GET    /v1/protocols/tracking-plans              — List all tracking plans in workspace
//	GET    /v1/protocols/tracking-plans/{id}         — Get a single tracking plan
//	PUT    /v1/protocols/tracking-plans/{id}         — Update a tracking plan
//	DELETE /v1/protocols/tracking-plans/{id}         — Delete a tracking plan
//	GET    /v1/protocols/tracking-plans/{id}/versions — Get version history
//	POST   /v1/protocols/tracking-plans/{id}/import  — Import tracking plan from CSV
//	GET    /v1/protocols/tracking-plans/{id}/export  — Export tracking plan as CSV
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewRouter creates a new chi router with all tracking plan management routes registered.
// The returned http.Handler can be mounted on a parent router (e.g., gateway's chi router)
// under the "/v1/protocols" prefix.
//
// The chi.Router returned by chi.NewRouter() implements http.Handler, so the function
// signature uses http.Handler as the return type following the pattern from
// services/rsources/http/http.go:23.
//
// Route table:
//
//	POST   /tracking-plans                     — Create a new tracking plan
//	GET    /tracking-plans                     — List all tracking plans in workspace
//	GET    /tracking-plans/{id}                — Get a single tracking plan
//	PUT    /tracking-plans/{id}                — Update a tracking plan
//	DELETE /tracking-plans/{id}                — Delete a tracking plan
//	GET    /tracking-plans/{id}/versions       — Get version history
//	POST   /tracking-plans/{id}/import         — Import tracking plan from CSV
//	GET    /tracking-plans/{id}/export         — Export tracking plan as CSV
//
// The {id} path parameter is extracted by handlers via chi.URLParam(r, "id"),
// consistent with the chi URL parameter pattern from warehouse/backfill/handler.go:161.
func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()

	// CRUD routes for tracking plans
	r.Post("/tracking-plans", h.CreateTrackingPlan)
	r.Get("/tracking-plans", h.ListTrackingPlans)
	r.Get("/tracking-plans/{id}", h.GetTrackingPlan)
	r.Put("/tracking-plans/{id}", h.UpdateTrackingPlan)
	r.Delete("/tracking-plans/{id}", h.DeleteTrackingPlan)

	// Version history
	r.Get("/tracking-plans/{id}/versions", h.GetVersionHistory)

	// CSV import/export
	r.Post("/tracking-plans/{id}/import", h.ImportCSV)
	r.Get("/tracking-plans/{id}/export", h.ExportCSV)

	return r
}
