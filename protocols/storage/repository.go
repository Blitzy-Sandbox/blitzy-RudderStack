// Package storage provides PostgreSQL persistence for tracking plans.
//
// This package implements a Repository for tracking plan CRUD operations,
// version history tracking, and workspace isolation. It is the foundational
// persistence layer consumed by protocols/api/ as part of the Tracking Plan
// Management API (E-024, Sprint 5-7).
//
// All public methods accept context.Context as first parameter for cancellation
// and timeout propagation. Workspace isolation is enforced on every query via
// a workspace_id predicate, ensuring multi-tenant safety.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// pkgLogger is the structured logger scoped to the protocols.storage namespace,
// following the warehouse/identity/identity.go logger hierarchy pattern.
var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("protocols").Child("storage")
}

// Sentinel errors for not-found conditions, following the
// ErrBackfillJobNotFound pattern from warehouse/backfill/repository.go.
var (
	// ErrTrackingPlanNotFound is returned when a tracking plan query yields no results.
	ErrTrackingPlanNotFound = errors.New("tracking plan not found")
	// ErrVersionNotFound is returned when a tracking plan version query yields no results.
	ErrVersionNotFound = errors.New("tracking plan version not found")
)

// Table name and column constants for SQL query construction.
const (
	trackingPlansTable        = "tracking_plans"
	trackingPlanVersionsTable = "tracking_plan_versions"

	trackingPlanColumns        = "id, workspace_id, name, schema, version, enforcement_config, created_at, updated_at"
	trackingPlanVersionColumns = "id, tracking_plan_id, version, schema, changelog, created_at"
)

// rawJSON wraps a []byte so that jsonrs.Marshal produces the raw JSON bytes
// instead of a base64-encoded string. Without this wrapper, jsonrs.Marshal([]byte)
// treats the data as an opaque byte slice and base64-encodes it, which is incorrect
// for JSONB column storage where the database expects raw JSON text.
// This follows the rawJSON pattern from warehouse/backfill/repository.go.
type rawJSON []byte

// MarshalJSON implements the json.Marshaler interface, returning the raw bytes
// as-is (they are already valid JSON) or "null" when the slice is empty/nil.
func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// TrackingPlan represents a row in the tracking_plans table.
// Schema holds the JSON Schema draft-07 definition stored as JSONB.
// EnforcementConfig holds the Block/Omit/Allow enforcement mode settings
// per source per call type, also stored as JSONB.
type TrackingPlan struct {
	ID                string    `json:"id"`
	WorkspaceID       string    `json:"workspace_id"`
	Name              string    `json:"name"`
	Schema            []byte    `json:"schema"`
	Version           int       `json:"version"`
	EnforcementConfig []byte    `json:"enforcement_config"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// TrackingPlanVersion represents a version snapshot in the tracking_plan_versions table.
// Each version captures the schema at a point in time along with a human-readable changelog.
type TrackingPlanVersion struct {
	ID             string    `json:"id"`
	TrackingPlanID string    `json:"tracking_plan_id"`
	Version        int       `json:"version"`
	Schema         []byte    `json:"schema"`
	Changelog      string    `json:"changelog"`
	CreatedAt      time.Time `json:"created_at"`
}

// Repository provides CRUD operations for the tracking_plans and
// tracking_plan_versions tables. It uses *sql.DB directly (standard library)
// since the protocols storage is a top-level package outside the warehouse subsystem.
type Repository struct {
	db *sql.DB
}

// NewRepository creates a new Repository with the given database handle.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// validateJSONBytes checks whether the provided byte slice contains valid JSON.
// It returns nil for empty/nil input (treated as optional). This prevents storing
// malformed JSON in PostgreSQL JSONB columns, catching issues at the application
// layer before they reach the database.
func validateJSONBytes(data []byte, fieldName string) error {
	if len(data) == 0 {
		return nil
	}
	var v any
	if err := jsonrs.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("invalid JSON in %s: %w", fieldName, err)
	}
	return nil
}

// marshalJSONField serialises a raw JSON byte slice through the rawJSON wrapper
// so that jsonrs.Marshal produces proper JSON text (not base64) suitable for
// PostgreSQL JSONB columns. Returns nil bytes for empty/nil input.
func marshalJSONField(data []byte, fieldName string) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	b, err := jsonrs.Marshal(rawJSON(data))
	if err != nil {
		return nil, fmt.Errorf("marshalling %s: %w", fieldName, err)
	}
	return b, nil
}

// Create inserts a new tracking plan into the tracking_plans table and returns
// the auto-generated ID. The created_at and updated_at timestamps are set to
// the current server time. Schema and EnforcementConfig are validated as valid
// JSON before insertion.
func (r *Repository) Create(ctx context.Context, tp TrackingPlan) (string, error) {
	pkgLogger.Debugn("Creating tracking plan",
		logger.NewStringField("workspaceID", tp.WorkspaceID),
		logger.NewStringField("name", tp.Name),
	)

	// Validate JSON fields before storage.
	if err := validateJSONBytes(tp.Schema, "schema"); err != nil {
		return "", fmt.Errorf("creating tracking plan: %w", err)
	}
	if err := validateJSONBytes(tp.EnforcementConfig, "enforcement_config"); err != nil {
		return "", fmt.Errorf("creating tracking plan: %w", err)
	}

	// Marshal JSON fields through rawJSON wrapper for proper JSONB storage.
	schemaBytes, err := marshalJSONField(tp.Schema, "schema")
	if err != nil {
		return "", fmt.Errorf("creating tracking plan: %w", err)
	}
	enforcementBytes, err := marshalJSONField(tp.EnforcementConfig, "enforcement_config")
	if err != nil {
		return "", fmt.Errorf("creating tracking plan: %w", err)
	}

	now := time.Now()
	var id string
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO `+trackingPlansTable+` (
			workspace_id, name, schema, version, enforcement_config, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		tp.WorkspaceID, tp.Name, schemaBytes, tp.Version, enforcementBytes, now, now,
	).Scan(&id)
	if err != nil {
		pkgLogger.Errorn("Error creating tracking plan", obskit.Error(err))
		return "", fmt.Errorf("creating tracking plan: %w", err)
	}
	return id, nil
}

// Get retrieves a single tracking plan by its primary key ID.
// Returns ErrTrackingPlanNotFound if no row exists with the given ID.
func (r *Repository) Get(ctx context.Context, id string) (TrackingPlan, error) {
	var tp TrackingPlan
	var schemaStr sql.NullString
	var enforcementStr sql.NullString

	err := r.db.QueryRowContext(ctx,
		`SELECT `+trackingPlanColumns+`
		FROM `+trackingPlansTable+`
		WHERE id = $1`,
		id,
	).Scan(
		&tp.ID, &tp.WorkspaceID, &tp.Name, &schemaStr,
		&tp.Version, &enforcementStr, &tp.CreatedAt, &tp.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TrackingPlan{}, ErrTrackingPlanNotFound
		}
		pkgLogger.Errorn("Error getting tracking plan",
			logger.NewStringField("id", id),
			obskit.Error(err),
		)
		return TrackingPlan{}, fmt.Errorf("getting tracking plan %s: %w", id, err)
	}
	if schemaStr.Valid {
		tp.Schema = []byte(schemaStr.String)
	}
	if enforcementStr.Valid {
		tp.EnforcementConfig = []byte(enforcementStr.String)
	}
	return tp, nil
}

// GetByWorkspace retrieves a single tracking plan scoped to a specific workspace.
// This enforces workspace isolation by requiring both workspace_id and id to match.
// Returns ErrTrackingPlanNotFound if no matching row exists.
func (r *Repository) GetByWorkspace(ctx context.Context, workspaceID string, id string) (TrackingPlan, error) {
	var tp TrackingPlan
	var schemaStr sql.NullString
	var enforcementStr sql.NullString

	err := r.db.QueryRowContext(ctx,
		`SELECT `+trackingPlanColumns+`
		FROM `+trackingPlansTable+`
		WHERE workspace_id = $1 AND id = $2`,
		workspaceID, id,
	).Scan(
		&tp.ID, &tp.WorkspaceID, &tp.Name, &schemaStr,
		&tp.Version, &enforcementStr, &tp.CreatedAt, &tp.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TrackingPlan{}, ErrTrackingPlanNotFound
		}
		pkgLogger.Errorn("Error getting tracking plan by workspace",
			logger.NewStringField("workspaceID", workspaceID),
			logger.NewStringField("id", id),
			obskit.Error(err),
		)
		return TrackingPlan{}, fmt.Errorf("getting tracking plan %s for workspace %s: %w", id, workspaceID, err)
	}
	if schemaStr.Valid {
		tp.Schema = []byte(schemaStr.String)
	}
	if enforcementStr.Valid {
		tp.EnforcementConfig = []byte(enforcementStr.String)
	}
	return tp, nil
}

// Update modifies an existing tracking plan identified by its ID and workspace_id.
// The update is workspace-scoped to enforce tenant isolation. Only name, schema,
// version, enforcement_config, and updated_at are modified — ID, workspace_id,
// and created_at remain immutable.
// Returns ErrTrackingPlanNotFound if no matching row exists.
func (r *Repository) Update(ctx context.Context, tp TrackingPlan) error {
	pkgLogger.Debugn("Updating tracking plan",
		logger.NewStringField("id", tp.ID),
		logger.NewStringField("workspaceID", tp.WorkspaceID),
	)

	// Validate JSON fields before storage.
	if err := validateJSONBytes(tp.Schema, "schema"); err != nil {
		return fmt.Errorf("updating tracking plan: %w", err)
	}
	if err := validateJSONBytes(tp.EnforcementConfig, "enforcement_config"); err != nil {
		return fmt.Errorf("updating tracking plan: %w", err)
	}

	// Marshal JSON fields through rawJSON wrapper for proper JSONB storage.
	schemaBytes, err := marshalJSONField(tp.Schema, "schema")
	if err != nil {
		return fmt.Errorf("updating tracking plan: %w", err)
	}
	enforcementBytes, err := marshalJSONField(tp.EnforcementConfig, "enforcement_config")
	if err != nil {
		return fmt.Errorf("updating tracking plan: %w", err)
	}

	now := time.Now()
	result, err := r.db.ExecContext(ctx,
		`UPDATE `+trackingPlansTable+`
		SET name = $1, schema = $2, version = $3, enforcement_config = $4, updated_at = $5
		WHERE id = $6 AND workspace_id = $7`,
		tp.Name, schemaBytes, tp.Version, enforcementBytes, now, tp.ID, tp.WorkspaceID,
	)
	if err != nil {
		pkgLogger.Errorn("Error updating tracking plan",
			logger.NewStringField("id", tp.ID),
			obskit.Error(err),
		)
		return fmt.Errorf("updating tracking plan %s: %w", tp.ID, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected for tracking plan %s: %w", tp.ID, err)
	}
	if rowsAffected == 0 {
		return ErrTrackingPlanNotFound
	}
	return nil
}

// Delete removes a tracking plan by its ID scoped to the given workspace.
// Associated versions are expected to be removed via ON DELETE CASCADE at the
// database migration level.
// Returns ErrTrackingPlanNotFound if no matching row exists.
func (r *Repository) Delete(ctx context.Context, workspaceID string, id string) error {
	pkgLogger.Debugn("Deleting tracking plan",
		logger.NewStringField("workspaceID", workspaceID),
		logger.NewStringField("id", id),
	)

	result, err := r.db.ExecContext(ctx,
		`DELETE FROM `+trackingPlansTable+`
		WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID,
	)
	if err != nil {
		pkgLogger.Errorn("Error deleting tracking plan",
			logger.NewStringField("id", id),
			obskit.Error(err),
		)
		return fmt.Errorf("deleting tracking plan %s: %w", id, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected for tracking plan %s: %w", id, err)
	}
	if rowsAffected == 0 {
		return ErrTrackingPlanNotFound
	}
	return nil
}

// List returns all tracking plans for the given workspace ordered by created_at
// descending (most recent first). Returns an empty non-nil slice if no tracking
// plans exist for the workspace, following the warehouse/backfill/repository.go
// ListBySource pattern.
func (r *Repository) List(ctx context.Context, workspaceID string) ([]TrackingPlan, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+trackingPlanColumns+`
		FROM `+trackingPlansTable+`
		WHERE workspace_id = $1
		ORDER BY created_at DESC`,
		workspaceID,
	)
	if err != nil {
		pkgLogger.Errorn("Error listing tracking plans",
			logger.NewStringField("workspaceID", workspaceID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("listing tracking plans for workspace %s: %w", workspaceID, err)
	}
	defer func() { _ = rows.Close() }()

	plans := make([]TrackingPlan, 0)
	for rows.Next() {
		var tp TrackingPlan
		var schemaStr sql.NullString
		var enforcementStr sql.NullString
		if err := rows.Scan(
			&tp.ID, &tp.WorkspaceID, &tp.Name, &schemaStr,
			&tp.Version, &enforcementStr, &tp.CreatedAt, &tp.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning tracking plan row: %w", err)
		}
		if schemaStr.Valid {
			tp.Schema = []byte(schemaStr.String)
		}
		if enforcementStr.Valid {
			tp.EnforcementConfig = []byte(enforcementStr.String)
		}
		plans = append(plans, tp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating tracking plan rows: %w", err)
	}
	return plans, nil
}

// CreateVersion inserts a new tracking plan version snapshot and returns the
// auto-generated ID. The created_at timestamp is set to the current server time.
func (r *Repository) CreateVersion(ctx context.Context, v TrackingPlanVersion) (string, error) {
	pkgLogger.Debugn("Creating tracking plan version",
		logger.NewStringField("trackingPlanID", v.TrackingPlanID),
	)

	// Validate schema JSON before storage.
	if err := validateJSONBytes(v.Schema, "schema"); err != nil {
		return "", fmt.Errorf("creating tracking plan version: %w", err)
	}

	// Marshal schema through rawJSON wrapper for proper JSONB storage.
	schemaBytes, err := marshalJSONField(v.Schema, "schema")
	if err != nil {
		return "", fmt.Errorf("creating tracking plan version: %w", err)
	}

	now := time.Now()
	var id string
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO `+trackingPlanVersionsTable+` (
			tracking_plan_id, version, schema, changelog, created_at
		) VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		v.TrackingPlanID, v.Version, schemaBytes, v.Changelog, now,
	).Scan(&id)
	if err != nil {
		pkgLogger.Errorn("Error creating tracking plan version",
			logger.NewStringField("trackingPlanID", v.TrackingPlanID),
			obskit.Error(err),
		)
		return "", fmt.Errorf("creating tracking plan version: %w", err)
	}
	return id, nil
}

// GetVersions returns all version snapshots for the given tracking plan,
// ordered by version number descending (most recent first). Returns an
// empty non-nil slice if no versions exist.
func (r *Repository) GetVersions(ctx context.Context, trackingPlanID string) ([]TrackingPlanVersion, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+trackingPlanVersionColumns+`
		FROM `+trackingPlanVersionsTable+`
		WHERE tracking_plan_id = $1
		ORDER BY version DESC`,
		trackingPlanID,
	)
	if err != nil {
		pkgLogger.Errorn("Error listing tracking plan versions",
			logger.NewStringField("trackingPlanID", trackingPlanID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("listing versions for tracking plan %s: %w", trackingPlanID, err)
	}
	defer func() { _ = rows.Close() }()

	versions := make([]TrackingPlanVersion, 0)
	for rows.Next() {
		var v TrackingPlanVersion
		var schemaStr sql.NullString
		if err := rows.Scan(
			&v.ID, &v.TrackingPlanID, &v.Version, &schemaStr,
			&v.Changelog, &v.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning tracking plan version row: %w", err)
		}
		if schemaStr.Valid {
			v.Schema = []byte(schemaStr.String)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating tracking plan version rows: %w", err)
	}
	return versions, nil
}

// GetVersion retrieves a specific version snapshot for a tracking plan.
// Returns ErrVersionNotFound if no matching version exists.
func (r *Repository) GetVersion(ctx context.Context, trackingPlanID string, version int) (TrackingPlanVersion, error) {
	var v TrackingPlanVersion
	var schemaStr sql.NullString

	err := r.db.QueryRowContext(ctx,
		`SELECT `+trackingPlanVersionColumns+`
		FROM `+trackingPlanVersionsTable+`
		WHERE tracking_plan_id = $1 AND version = $2`,
		trackingPlanID, version,
	).Scan(
		&v.ID, &v.TrackingPlanID, &v.Version, &schemaStr,
		&v.Changelog, &v.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TrackingPlanVersion{}, ErrVersionNotFound
		}
		pkgLogger.Errorn("Error getting tracking plan version",
			logger.NewStringField("trackingPlanID", trackingPlanID),
			obskit.Error(err),
		)
		return TrackingPlanVersion{}, fmt.Errorf("getting version %d for tracking plan %s: %w", version, trackingPlanID, err)
	}
	if schemaStr.Valid {
		v.Schema = []byte(schemaStr.String)
	}
	return v, nil
}
