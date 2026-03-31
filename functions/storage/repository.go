// Package storage provides a PostgreSQL-backed persistence layer for user-defined
// function definitions in the RudderStack Functions management system (E-018).
// It supports CRUD operations with automatic version tracking for Source Functions,
// Destination Functions, and Insert Functions.
//
// The repository follows patterns established in warehouse/backfill/repository.go,
// using *sql.DB directly (not warehouse sqlmiddleware) and rudder-go-kit/logger
// for structured logging.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rudderlabs/rudder-go-kit/logger"
)

const (
	// functionsTableName is the database table name for storing function definitions.
	functionsTableName = "functions"

	// functionsColumns lists all columns in the functions table for SELECT queries.
	// Order must match the Scan() call sites in scanFunction, Get, List, and GetByVersion.
	functionsColumns = `id, workspace_id, name, type, code, version, settings, created_at, updated_at`

	// defaultListLimit is the maximum number of results returned by List when
	// ListOptions.Limit is zero or negative.
	defaultListLimit = 100
)

// ErrFunctionNotFound is returned when a requested function does not exist
// in the functions table. Callers should use errors.Is to check for this sentinel.
var ErrFunctionNotFound = errors.New("function not found")

// Function represents a user-defined function stored in the database.
// Functions can be of type "source", "destination", or "insert", corresponding
// to Source Functions (onRequest handler), Destination Functions (typed event handlers),
// and Insert Functions (pre-destination transformation hooks).
type Function struct {
	// ID is a string-based primary key (UUID or user-provided identifier).
	ID string `json:"id"`

	// WorkspaceID scopes the function to a multi-tenant workspace.
	WorkspaceID string `json:"workspaceId"`

	// Name is the human-readable function name.
	Name string `json:"name"`

	// Type is one of "source", "destination", or "insert".
	Type string `json:"type"`

	// Code contains the JavaScript function code stored as text.
	Code string `json:"code"`

	// Version is an integer version number, starting at 1 and auto-incremented on update.
	Version int `json:"version"`

	// Settings holds arbitrary JSON configuration for the function.
	// Stored as a JSONB column in PostgreSQL.
	Settings json.RawMessage `json:"settings"`

	// CreatedAt is the timestamp when the function was first created.
	CreatedAt time.Time `json:"createdAt"`

	// UpdatedAt is the timestamp when the function was last updated.
	UpdatedAt time.Time `json:"updatedAt"`
}

// ListOptions configures pagination and filtering for function listing.
type ListOptions struct {
	// Limit is the maximum number of results to return. When zero or negative,
	// defaults to 100.
	Limit int

	// Offset is the number of results to skip for pagination.
	Offset int

	// TypeFilter filters results by function type ("source", "destination", "insert").
	// An empty string means no type filter is applied.
	TypeFilter string
}

// Repository provides CRUD operations for the functions table.
// It follows the repository pattern established in warehouse/backfill/repository.go,
// using *sql.DB for PostgreSQL access and rudder-go-kit/logger for structured logging.
type Repository struct {
	db  *sql.DB
	log logger.Logger
}

// New creates a new Repository with the given database handle and logger.
// The logger is wrapped in a child logger named "functions_storage" following
// the scoped child logger pattern from AAP Rule 0.7.4.
func New(db *sql.DB, log logger.Logger) *Repository {
	return &Repository{
		db:  db,
		log: log.Child("functions_storage"),
	}
}

// Create inserts a new function record into the functions table.
// The function's Version is set to 1 and CreatedAt/UpdatedAt are set to the
// current time. If Settings is nil, it is stored as NULL in the database.
func (r *Repository) Create(ctx context.Context, fn *Function) error {
	now := time.Now()
	fn.Version = 1
	fn.CreatedAt = now
	fn.UpdatedAt = now

	// Convert nil Settings to a database-friendly value.
	// sql.DB handles []byte(nil) as SQL NULL for JSONB columns.
	var settingsArg interface{}
	if fn.Settings != nil {
		settingsArg = []byte(fn.Settings)
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO `+functionsTableName+` (`+functionsColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		fn.ID,
		fn.WorkspaceID,
		fn.Name,
		fn.Type,
		fn.Code,
		fn.Version,
		settingsArg,
		fn.CreatedAt,
		fn.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("creating function: %w", err)
	}

	r.log.Debugn("created function",
		logger.NewStringField("functionID", fn.ID),
		logger.NewStringField("workspaceID", fn.WorkspaceID),
		logger.NewStringField("type", fn.Type),
	)
	return nil
}

// Get retrieves a function by its primary key ID scoped to a workspace.
// The workspaceID filter provides defense-in-depth multi-tenant isolation,
// ensuring callers can only access functions within their own workspace
// (consistent with protocols/storage which scopes all operations by workspace).
// Returns ErrFunctionNotFound if no row exists with the given ID and workspace.
func (r *Repository) Get(ctx context.Context, id string, workspaceID string) (*Function, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+functionsColumns+`
		FROM `+functionsTableName+`
		WHERE id = $1 AND workspace_id = $2`,
		id,
		workspaceID,
	)

	fn, err := scanFunction(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFunctionNotFound
		}
		return nil, fmt.Errorf("getting function %s: %w", id, err)
	}
	return fn, nil
}

// Update modifies an existing function and increments its version number.
// The function's Version is atomically incremented by 1 in the database and
// UpdatedAt is set to the current time. On success, fn.Version and fn.UpdatedAt
// are updated to reflect the new values.
// Returns ErrFunctionNotFound if no row exists with the given ID.
func (r *Repository) Update(ctx context.Context, fn *Function) error {
	now := time.Now()

	// Convert nil Settings to a database-friendly value.
	var settingsArg interface{}
	if fn.Settings != nil {
		settingsArg = []byte(fn.Settings)
	}

	// Use RETURNING to atomically retrieve the new version and timestamp,
	// avoiding a separate SELECT round-trip. The workspace_id filter provides
	// defense-in-depth multi-tenant isolation, consistent with protocols/storage.
	var newVersion int
	var newUpdatedAt time.Time
	err := r.db.QueryRowContext(ctx,
		`UPDATE `+functionsTableName+`
		SET name = $1, type = $2, code = $3, version = version + 1,
			settings = $4, updated_at = $5
		WHERE id = $6 AND workspace_id = $7
		RETURNING version, updated_at`,
		fn.Name,
		fn.Type,
		fn.Code,
		settingsArg,
		now,
		fn.ID,
		fn.WorkspaceID,
	).Scan(&newVersion, &newUpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFunctionNotFound
		}
		return fmt.Errorf("updating function %s: %w", fn.ID, err)
	}

	// Reflect the database state back onto the caller's struct.
	fn.Version = newVersion
	fn.UpdatedAt = newUpdatedAt

	r.log.Debugn("updated function",
		logger.NewStringField("functionID", fn.ID),
		logger.NewIntField("version", int64(fn.Version)),
	)
	return nil
}

// Delete removes a function from the functions table by its primary key ID,
// scoped to a workspace. The workspaceID filter provides defense-in-depth
// multi-tenant isolation, consistent with protocols/storage.
// Returns ErrFunctionNotFound if no row exists with the given ID and workspace.
func (r *Repository) Delete(ctx context.Context, id string, workspaceID string) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM `+functionsTableName+`
		WHERE id = $1 AND workspace_id = $2`,
		id,
		workspaceID,
	)
	if err != nil {
		return fmt.Errorf("deleting function %s: %w", id, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected for function %s: %w", id, err)
	}
	if rowsAffected == 0 {
		return ErrFunctionNotFound
	}

	r.log.Debugn("deleted function",
		logger.NewStringField("functionID", id),
	)
	return nil
}

// List retrieves functions for a workspace with optional filtering and pagination.
// Results are ordered by created_at descending (newest first).
// Returns an empty (non-nil) slice if no functions match the criteria.
func (r *Repository) List(ctx context.Context, workspaceID string, opts ListOptions) ([]*Function, error) {
	// Build the query dynamically based on filter options.
	query := `SELECT ` + functionsColumns + `
		FROM ` + functionsTableName + `
		WHERE workspace_id = $1`

	args := []interface{}{workspaceID}
	paramIdx := 2

	if opts.TypeFilter != "" {
		query += fmt.Sprintf(` AND type = $%d`, paramIdx)
		args = append(args, opts.TypeFilter)
		paramIdx++
	}

	query += ` ORDER BY created_at DESC`

	// Apply limit with a sensible default.
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	query += fmt.Sprintf(` LIMIT $%d`, paramIdx)
	args = append(args, limit)
	paramIdx++

	// Apply offset when positive.
	if opts.Offset > 0 {
		query += fmt.Sprintf(` OFFSET $%d`, paramIdx)
		args = append(args, opts.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing functions for workspace %s: %w", workspaceID, err)
	}
	defer func() { _ = rows.Close() }()

	functions := make([]*Function, 0)
	for rows.Next() {
		fn, err := scanFunction(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning function row: %w", err)
		}
		functions = append(functions, fn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating function rows: %w", err)
	}

	return functions, nil
}

// GetByVersion retrieves a specific version of a function by its ID and version number.
// Returns ErrFunctionNotFound if no row exists with the given ID and version combination.
//
// In the current single-table schema, the version column tracks the latest version
// on the same row. This method validates that the stored version matches the requested
// version. A future version-history table would extend this to support historical lookups.
func (r *Repository) GetByVersion(ctx context.Context, id string, version int, workspaceID string) (*Function, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+functionsColumns+`
		FROM `+functionsTableName+`
		WHERE id = $1 AND version = $2 AND workspace_id = $3`,
		id,
		version,
		workspaceID,
	)

	fn, err := scanFunction(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFunctionNotFound
		}
		return nil, fmt.Errorf("getting function %s version %d: %w", id, version, err)
	}
	return fn, nil
}

// scanner is an interface satisfied by both *sql.Row and *sql.Rows,
// allowing scanFunction to work with single-row and multi-row queries.
type scanner interface {
	Scan(dest ...any) error
}

// scanFunction scans a single row into a Function struct, handling the
// nullable Settings JSON column via sql.NullString conversion.
// The scan order must match the functionsColumns constant exactly.
func scanFunction(s scanner) (*Function, error) {
	var fn Function
	var settings sql.NullString

	err := s.Scan(
		&fn.ID,
		&fn.WorkspaceID,
		&fn.Name,
		&fn.Type,
		&fn.Code,
		&fn.Version,
		&settings,
		&fn.CreatedAt,
		&fn.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	if settings.Valid {
		fn.Settings = json.RawMessage(settings.String)
	}

	return &fn, nil
}
