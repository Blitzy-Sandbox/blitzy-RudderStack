// Package api contains the service adapter that bridges protocols/storage.Repository
// to the TrackingPlanService interface required by the HTTP handler. This follows
// the same adapter pattern used in warehouse/api/http.go where a service layer
// converts between HTTP-layer request/response types and storage-layer domain types.
package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"

	"github.com/rudderlabs/rudder-server/protocols/storage"
)

// ---------------------------------------------------------------------------
// Repository Interface (for testability)
// ---------------------------------------------------------------------------

// trackingPlanRepository defines the minimal storage operations the service
// needs. It is satisfied by *storage.Repository.
type trackingPlanRepository interface {
	Create(ctx context.Context, tp storage.TrackingPlan) (string, error)
	GetByWorkspace(ctx context.Context, workspaceID, id string) (storage.TrackingPlan, error)
	Update(ctx context.Context, tp storage.TrackingPlan) error
	Delete(ctx context.Context, workspaceID, id string) error
	List(ctx context.Context, workspaceID string, limit, offset int) ([]storage.TrackingPlan, error)
	GetVersions(ctx context.Context, trackingPlanID string) ([]storage.TrackingPlanVersion, error)
	CreateVersion(ctx context.Context, v storage.TrackingPlanVersion) (string, error)
}

// ---------------------------------------------------------------------------
// Service Implementation
// ---------------------------------------------------------------------------

// Service adapts a protocols/storage.Repository to the TrackingPlanService
// interface expected by the HTTP handler. It translates between the API-layer
// request/response types and the storage-layer domain types.
type Service struct {
	repo trackingPlanRepository
}

// NewService creates a new Service wrapping the given repository.
func NewService(repo trackingPlanRepository) *Service {
	return &Service{repo: repo}
}

// Create creates a new tracking plan and returns the generated ID.
func (s *Service) Create(ctx context.Context, workspaceID string, req CreateTrackingPlanRequest) (string, error) {
	tp := storage.TrackingPlan{
		WorkspaceID:       workspaceID,
		Name:              req.Name,
		Schema:            req.Schema,
		Version:           1,
		EnforcementConfig: req.EnforcementConfig,
	}
	id, err := s.repo.Create(ctx, tp)
	if err != nil {
		return "", fmt.Errorf("creating tracking plan: %w", err)
	}
	// Create initial version snapshot.
	_, vErr := s.repo.CreateVersion(ctx, storage.TrackingPlanVersion{
		TrackingPlanID: id,
		Version:        1,
		Schema:         req.Schema,
		Changelog:      "Initial version",
	})
	if vErr != nil {
		// Log but do not fail — the plan itself was created successfully.
		return id, nil //nolint:nilerr // version snapshot failure is non-critical
	}
	return id, nil
}

// Get retrieves a single tracking plan by workspace and ID.
func (s *Service) Get(ctx context.Context, workspaceID, id string) (*TrackingPlanResponse, error) {
	tp, err := s.repo.GetByWorkspace(ctx, workspaceID, id)
	if err != nil {
		if errors.Is(err, storage.ErrTrackingPlanNotFound) {
			return nil, ErrTrackingPlanNotFound
		}
		return nil, fmt.Errorf("getting tracking plan: %w", err)
	}
	resp := toTrackingPlanResponse(tp)
	return &resp, nil
}

// Update updates an existing tracking plan (creates a new version).
func (s *Service) Update(ctx context.Context, workspaceID, id string, req UpdateTrackingPlanRequest) error {
	// Fetch existing to increment version.
	existing, err := s.repo.GetByWorkspace(ctx, workspaceID, id)
	if err != nil {
		if errors.Is(err, storage.ErrTrackingPlanNotFound) {
			return ErrTrackingPlanNotFound
		}
		return fmt.Errorf("fetching tracking plan for update: %w", err)
	}

	newVersion := existing.Version + 1
	tp := storage.TrackingPlan{
		ID:                id,
		WorkspaceID:       workspaceID,
		Name:              req.Name,
		Schema:            req.Schema,
		Version:           newVersion,
		EnforcementConfig: req.EnforcementConfig,
	}
	// Preserve original values when not provided in update.
	if tp.Name == "" {
		tp.Name = existing.Name
	}
	if len(tp.Schema) == 0 {
		tp.Schema = existing.Schema
	}
	if len(tp.EnforcementConfig) == 0 {
		tp.EnforcementConfig = existing.EnforcementConfig
	}

	if err := s.repo.Update(ctx, tp); err != nil {
		return fmt.Errorf("updating tracking plan: %w", err)
	}

	// Create version snapshot.
	changelog := req.Changelog
	if changelog == "" {
		changelog = fmt.Sprintf("Version %d", newVersion)
	}
	_, _ = s.repo.CreateVersion(ctx, storage.TrackingPlanVersion{
		TrackingPlanID: id,
		Version:        newVersion,
		Schema:         tp.Schema,
		Changelog:      changelog,
	})
	return nil
}

// Delete removes a tracking plan and all its versions.
func (s *Service) Delete(ctx context.Context, workspaceID, id string) error {
	err := s.repo.Delete(ctx, workspaceID, id)
	if err != nil {
		if errors.Is(err, storage.ErrTrackingPlanNotFound) {
			return ErrTrackingPlanNotFound
		}
		return fmt.Errorf("deleting tracking plan: %w", err)
	}
	return nil
}

// List returns tracking plans for a workspace with pagination support.
// If limit is <= 0, the storage layer applies a default of 100.
// If offset is < 0, it defaults to 0.
func (s *Service) List(ctx context.Context, workspaceID string, limit, offset int) ([]TrackingPlanResponse, error) {
	plans, err := s.repo.List(ctx, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("listing tracking plans: %w", err)
	}
	result := make([]TrackingPlanResponse, 0, len(plans))
	for _, tp := range plans {
		result = append(result, toTrackingPlanResponse(tp))
	}
	return result, nil
}

// GetVersions returns the version history for a tracking plan.
func (s *Service) GetVersions(ctx context.Context, workspaceID, trackingPlanID string) ([]TrackingPlanVersionResponse, error) {
	// Verify plan belongs to workspace.
	_, err := s.repo.GetByWorkspace(ctx, workspaceID, trackingPlanID)
	if err != nil {
		if errors.Is(err, storage.ErrTrackingPlanNotFound) {
			return nil, ErrTrackingPlanNotFound
		}
		return nil, fmt.Errorf("verifying tracking plan ownership: %w", err)
	}
	versions, err := s.repo.GetVersions(ctx, trackingPlanID)
	if err != nil {
		return nil, fmt.Errorf("getting versions: %w", err)
	}
	result := make([]TrackingPlanVersionResponse, 0, len(versions))
	for _, v := range versions {
		result = append(result, toTrackingPlanVersionResponse(v))
	}
	return result, nil
}

// ImportCSV imports a tracking plan schema from CSV data. The CSV is expected
// to have columns: event_name, property_name, property_type, required, description.
// The data is parsed and converted into a JSON Schema draft-07 structure.
func (s *Service) ImportCSV(ctx context.Context, workspaceID, trackingPlanID string, csvData []byte) error {
	// Verify plan belongs to workspace.
	existing, err := s.repo.GetByWorkspace(ctx, workspaceID, trackingPlanID)
	if err != nil {
		if errors.Is(err, storage.ErrTrackingPlanNotFound) {
			return ErrTrackingPlanNotFound
		}
		return fmt.Errorf("verifying tracking plan for CSV import: %w", err)
	}

	schema, parseErr := csvToJSONSchema(csvData)
	if parseErr != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCSV, parseErr)
	}

	newVersion := existing.Version + 1
	tp := storage.TrackingPlan{
		ID:                trackingPlanID,
		WorkspaceID:       workspaceID,
		Name:              existing.Name,
		Schema:            schema,
		Version:           newVersion,
		EnforcementConfig: existing.EnforcementConfig,
	}
	if err := s.repo.Update(ctx, tp); err != nil {
		return fmt.Errorf("updating plan from CSV: %w", err)
	}
	_, _ = s.repo.CreateVersion(ctx, storage.TrackingPlanVersion{
		TrackingPlanID: trackingPlanID,
		Version:        newVersion,
		Schema:         schema,
		Changelog:      "Imported from CSV",
	})
	return nil
}

// ExportCSV exports a tracking plan schema as CSV data. The CSV contains columns:
// event_name, property_name, property_type, required, description.
func (s *Service) ExportCSV(ctx context.Context, workspaceID, trackingPlanID string) ([]byte, error) {
	tp, err := s.repo.GetByWorkspace(ctx, workspaceID, trackingPlanID)
	if err != nil {
		if errors.Is(err, storage.ErrTrackingPlanNotFound) {
			return nil, ErrTrackingPlanNotFound
		}
		return nil, fmt.Errorf("getting tracking plan for CSV export: %w", err)
	}
	csvBytes, err := jsonSchemaToCSV(tp.Schema)
	if err != nil {
		return nil, fmt.Errorf("exporting schema to CSV: %w", err)
	}
	return csvBytes, nil
}

// ---------------------------------------------------------------------------
// Conversion Helpers
// ---------------------------------------------------------------------------

func toTrackingPlanResponse(tp storage.TrackingPlan) TrackingPlanResponse {
	return TrackingPlanResponse{
		ID:                tp.ID,
		WorkspaceID:       tp.WorkspaceID,
		Name:              tp.Name,
		Schema:            tp.Schema,
		Version:           tp.Version,
		EnforcementConfig: tp.EnforcementConfig,
		CreatedAt:         tp.CreatedAt,
		UpdatedAt:         tp.UpdatedAt,
	}
}

func toTrackingPlanVersionResponse(v storage.TrackingPlanVersion) TrackingPlanVersionResponse {
	return TrackingPlanVersionResponse{
		ID:             v.ID,
		TrackingPlanID: v.TrackingPlanID,
		Version:        v.Version,
		Schema:         v.Schema,
		Changelog:      v.Changelog,
		CreatedAt:      v.CreatedAt,
	}
}

// csvToJSONSchema converts CSV data into a JSON Schema draft-07 structure.
// Expected columns: event_name, property_name, property_type, required, description.
func csvToJSONSchema(data []byte) ([]byte, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}
	// Normalise header.
	colIdx := make(map[string]int)
	for i, h := range header {
		colIdx[strings.TrimSpace(strings.ToLower(h))] = i
	}
	requiredCols := []string{"event_name", "property_name", "property_type"}
	for _, c := range requiredCols {
		if _, ok := colIdx[c]; !ok {
			return nil, fmt.Errorf("missing required CSV column: %s", c)
		}
	}

	// eventSchemas: event_name → list of property defs.
	type propDef struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Required    bool   `json:"required"`
		Description string `json:"description,omitempty"`
	}
	eventSchemas := make(map[string][]propDef)

	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading CSV record: %w", err)
		}
		evtName := strings.TrimSpace(record[colIdx["event_name"]])
		propName := strings.TrimSpace(record[colIdx["property_name"]])
		propType := strings.TrimSpace(record[colIdx["property_type"]])
		isRequired := false
		if idx, ok := colIdx["required"]; ok && idx < len(record) {
			isRequired, _ = strconv.ParseBool(strings.TrimSpace(record[idx]))
		}
		desc := ""
		if idx, ok := colIdx["description"]; ok && idx < len(record) {
			desc = strings.TrimSpace(record[idx])
		}
		eventSchemas[evtName] = append(eventSchemas[evtName], propDef{
			Name:        propName,
			Type:        propType,
			Required:    isRequired,
			Description: desc,
		})
	}

	// Build JSON Schema.
	type jsonSchemaEvent struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required,omitempty"`
	}
	schema := map[string]any{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type":    "object",
	}
	events := make(map[string]jsonSchemaEvent)
	for evtName, props := range eventSchemas {
		evt := jsonSchemaEvent{
			Type:       "object",
			Properties: make(map[string]any),
		}
		for _, p := range props {
			propSchema := map[string]any{"type": p.Type}
			if p.Description != "" {
				propSchema["description"] = p.Description
			}
			evt.Properties[p.Name] = propSchema
			if p.Required {
				evt.Required = append(evt.Required, p.Name)
			}
		}
		events[evtName] = evt
	}
	schema["events"] = events
	b, err := jsonrs.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshalling schema: %w", err)
	}
	return b, nil
}

// jsonSchemaToCSV converts a JSON Schema into CSV data with columns:
// event_name, property_name, property_type, required, description.
func jsonSchemaToCSV(schema []byte) ([]byte, error) {
	if len(schema) == 0 {
		return []byte("event_name,property_name,property_type,required,description\n"), nil
	}
	var parsed map[string]any
	if err := jsonrs.Unmarshal(schema, &parsed); err != nil {
		return nil, fmt.Errorf("parsing schema JSON: %w", err)
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"event_name", "property_name", "property_type", "required", "description"})

	eventsRaw, ok := parsed["events"]
	if !ok {
		w.Flush()
		return buf.Bytes(), nil
	}
	events, ok := eventsRaw.(map[string]any)
	if !ok {
		w.Flush()
		return buf.Bytes(), nil
	}

	for evtName, evtRaw := range events {
		evt, ok := evtRaw.(map[string]any)
		if !ok {
			continue
		}
		propsRaw, ok := evt["properties"]
		if !ok {
			continue
		}
		props, ok := propsRaw.(map[string]any)
		if !ok {
			continue
		}
		requiredSet := make(map[string]bool)
		if reqRaw, ok := evt["required"]; ok {
			if reqArr, ok := reqRaw.([]any); ok {
				for _, r := range reqArr {
					if s, ok := r.(string); ok {
						requiredSet[s] = true
					}
				}
			}
		}
		for propName, propRaw := range props {
			prop, ok := propRaw.(map[string]any)
			if !ok {
				continue
			}
			propType := ""
			if t, ok := prop["type"].(string); ok {
				propType = t
			}
			desc := ""
			if d, ok := prop["description"].(string); ok {
				desc = d
			}
			isReq := "false"
			if requiredSet[propName] {
				isReq = "true"
			}
			_ = w.Write([]string{evtName, propName, propType, isReq, desc})
		}
	}
	w.Flush()
	return buf.Bytes(), nil
}

// Compile-time interface verification.
var _ TrackingPlanService = (*Service)(nil)
