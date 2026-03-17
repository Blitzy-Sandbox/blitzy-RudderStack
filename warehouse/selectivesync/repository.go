// Package selectivesync provides per-table and per-column sync filtering
// for warehouse destinations, allowing users to include or exclude specific
// tables and columns from warehouse sync operations.
package selectivesync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/utils/timeutil"
	sqlmiddleware "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
)

// Table and column constants for the wh_selective_sync table.
const (
	selectiveSyncTableName = "wh_selective_sync"
	selectiveSyncColumns   = `
		id,
		source_id,
		destination_id,
		workspace_id,
		excluded_tables,
		excluded_columns,
		created_at,
		updated_at
	`
)

// Repository provides CRUD operations for the wh_selective_sync table.
// It follows the repository pattern established in warehouse/internal/repo/,
// using sqlquerywrapper.DB for database access, functional options for
// configuration, and TimerStat instrumentation for query duration metrics.
//
// The wh_selective_sync table has a unique constraint on (source_id, destination_id),
// enabling atomic upsert semantics via PostgreSQL ON CONFLICT.
type Repository struct {
	db           *sqlmiddleware.DB
	now          func() time.Time
	statsFactory stats.Stats
}

// RepositoryOption is a functional option for configuring the Repository.
// It follows the functional options pattern from warehouse/internal/repo/options.go.
type RepositoryOption func(*Repository)

// WithNow injects a custom clock function for deterministic testing.
// When provided, the repository uses this function instead of timeutil.Now
// to generate timestamps for created_at and updated_at fields.
func WithNow(now func() time.Time) RepositoryOption {
	return func(r *Repository) {
		r.now = now
	}
}

// WithStats injects a custom stats factory for instrumentation.
// When provided, the repository uses this factory to emit query duration
// metrics via NewTaggedStat with TimerType.
func WithStats(s stats.Stats) RepositoryOption {
	return func(r *Repository) {
		r.statsFactory = s
	}
}

// NewRepository creates a new selective sync repository backed by the given
// database connection. It applies the provided functional options, defaulting
// to timeutil.Now for timestamps and stats.NOP for metrics instrumentation.
//
// Usage:
//
//	repo := selectivesync.NewRepository(db)
//	repo := selectivesync.NewRepository(db, selectivesync.WithStats(statsFactory))
//	repo := selectivesync.NewRepository(db, selectivesync.WithNow(fixedClock))
func NewRepository(db *sqlmiddleware.DB, opts ...RepositoryOption) *Repository {
	r := &Repository{
		db:           db,
		now:          timeutil.Now,
		statsFactory: stats.NOP,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// timerStat returns a deferred function that records the duration of a database
// action as a Prometheus histogram metric. It follows the TimerStat pattern
// from warehouse/internal/repo/repo.go.
//
// The metric name follows the convention:
//
//	warehouse_repo_query_selective_sync_<action>_duration_seconds
func (r *Repository) timerStat(action string, extraTags stats.Tags) func() {
	statName := "warehouse_repo_query_selective_sync_" + action + "_duration_seconds"
	return r.statsFactory.NewTaggedStat(statName, stats.TimerType, extraTags).RecordDuration()
}

// Upsert inserts or updates a selective sync configuration for a source/destination
// pair. It uses PostgreSQL ON CONFLICT on the unique (source_id, destination_id)
// constraint to perform an atomic upsert — inserting a new row if no conflict
// exists, or updating workspace_id, excluded_tables, excluded_columns, and
// updated_at if a row with the same source/destination already exists.
//
// The ExcludedTables ([]string) and ExcludedColumns (map[string][]string) fields
// are serialized to JSONB using jsonrs.Marshal (never encoding/json).
//
// Timestamps are generated using the injected clock function (r.now()).
func (r *Repository) Upsert(ctx context.Context, cfg SelectiveSyncConfig) error {
	defer r.timerStat("upsert", stats.Tags{
		"sourceId":      cfg.SourceID,
		"destinationId": cfg.DestinationID,
	})()

	// Ensure non-nil slices/maps for JSONB serialization to avoid null values.
	excludedTables := cfg.ExcludedTables
	if excludedTables == nil {
		excludedTables = []string{}
	}
	excludedColumns := cfg.ExcludedColumns
	if excludedColumns == nil {
		excludedColumns = map[string][]string{}
	}

	excludedTablesJSON, err := jsonrs.Marshal(excludedTables)
	if err != nil {
		return fmt.Errorf("marshalling excluded_tables: %w", err)
	}
	excludedColumnsJSON, err := jsonrs.Marshal(excludedColumns)
	if err != nil {
		return fmt.Errorf("marshalling excluded_columns: %w", err)
	}

	now := r.now()
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO `+selectiveSyncTableName+` (
			source_id, destination_id, workspace_id,
			excluded_tables, excluded_columns,
			created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (source_id, destination_id) DO UPDATE SET
			workspace_id = EXCLUDED.workspace_id,
			excluded_tables = EXCLUDED.excluded_tables,
			excluded_columns = EXCLUDED.excluded_columns,
			updated_at = EXCLUDED.updated_at
	`,
		cfg.SourceID, cfg.DestinationID, cfg.WorkspaceID,
		excludedTablesJSON, excludedColumnsJSON,
		now, now,
	)
	if err != nil {
		return fmt.Errorf("upserting selective sync config: %w", err)
	}
	return nil
}

// Get retrieves the selective sync configuration for a specific source/destination
// pair. Returns a pointer to SelectiveSyncConfig on success.
//
// If no configuration exists for the given source/destination pair, the method
// returns ErrSelectiveSyncNotFound (defined in model.go). This sentinel error
// allows callers to distinguish "not found" from other database errors using
// errors.Is().
//
// The JSONB columns (excluded_tables, excluded_columns) are deserialized using
// jsonrs.Unmarshal (never encoding/json).
func (r *Repository) Get(ctx context.Context, sourceID, destID string) (*SelectiveSyncConfig, error) {
	defer r.timerStat("get", stats.Tags{
		"sourceId":      sourceID,
		"destinationId": destID,
	})()

	var cfg SelectiveSyncConfig
	var excludedTablesJSON, excludedColumnsJSON []byte

	err := r.db.QueryRowContext(ctx, `
		SELECT `+selectiveSyncColumns+`
		FROM `+selectiveSyncTableName+`
		WHERE source_id = $1 AND destination_id = $2
	`, sourceID, destID).Scan(
		&cfg.ID, &cfg.SourceID, &cfg.DestinationID, &cfg.WorkspaceID,
		&excludedTablesJSON, &excludedColumnsJSON,
		&cfg.CreatedAt, &cfg.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSelectiveSyncNotFound
		}
		return nil, fmt.Errorf("getting selective sync config: %w", err)
	}

	// Unmarshal JSONB columns using jsonrs (CRITICAL: never encoding/json).
	if len(excludedTablesJSON) > 0 {
		if err := jsonrs.Unmarshal(excludedTablesJSON, &cfg.ExcludedTables); err != nil {
			return nil, fmt.Errorf("unmarshalling excluded_tables: %w", err)
		}
	}
	if len(excludedColumnsJSON) > 0 {
		if err := jsonrs.Unmarshal(excludedColumnsJSON, &cfg.ExcludedColumns); err != nil {
			return nil, fmt.Errorf("unmarshalling excluded_columns: %w", err)
		}
	}

	return &cfg, nil
}

// Delete removes the selective sync configuration for a source/destination pair.
//
// This operation is idempotent: deleting a configuration that does not exist
// returns nil (no error). This design choice aligns with RESTful DELETE semantics
// and simplifies callers that perform "ensure-deleted" operations.
func (r *Repository) Delete(ctx context.Context, sourceID, destID string) error {
	defer r.timerStat("delete", stats.Tags{
		"sourceId":      sourceID,
		"destinationId": destID,
	})()

	_, err := r.db.ExecContext(ctx, `
		DELETE FROM `+selectiveSyncTableName+`
		WHERE source_id = $1 AND destination_id = $2
	`, sourceID, destID)
	if err != nil {
		return fmt.Errorf("deleting selective sync config: %w", err)
	}
	return nil
}

// ListByWorkspace retrieves all selective sync configurations belonging to the
// specified workspace, ordered by creation time (newest first).
//
// The method returns an empty slice ([]SelectiveSyncConfig{}, not nil) when no
// configurations exist for the workspace. This ensures callers can safely iterate
// over the result without nil checks and produce valid JSON arrays ([]) when
// serialized.
//
// Each returned configuration has its JSONB columns (excluded_tables, excluded_columns)
// deserialized using jsonrs.Unmarshal.
func (r *Repository) ListByWorkspace(ctx context.Context, workspaceID string) ([]SelectiveSyncConfig, error) {
	defer r.timerStat("list_by_workspace", stats.Tags{"workspaceId": workspaceID})()

	rows, err := r.db.QueryContext(ctx, `
		SELECT `+selectiveSyncColumns+`
		FROM `+selectiveSyncTableName+`
		WHERE workspace_id = $1
		ORDER BY created_at DESC
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("listing selective sync configs by workspace: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Initialize to empty slice (not nil) to ensure JSON serialization produces []
	// instead of null, and callers can safely iterate without nil checks.
	configs := make([]SelectiveSyncConfig, 0)
	for rows.Next() {
		var cfg SelectiveSyncConfig
		var excludedTablesJSON, excludedColumnsJSON []byte

		if err := rows.Scan(
			&cfg.ID, &cfg.SourceID, &cfg.DestinationID, &cfg.WorkspaceID,
			&excludedTablesJSON, &excludedColumnsJSON,
			&cfg.CreatedAt, &cfg.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning selective sync config: %w", err)
		}

		// Unmarshal JSONB columns using jsonrs (CRITICAL: never encoding/json).
		if len(excludedTablesJSON) > 0 {
			if err := jsonrs.Unmarshal(excludedTablesJSON, &cfg.ExcludedTables); err != nil {
				return nil, fmt.Errorf("unmarshalling excluded_tables: %w", err)
			}
		}
		if len(excludedColumnsJSON) > 0 {
			if err := jsonrs.Unmarshal(excludedColumnsJSON, &cfg.ExcludedColumns); err != nil {
				return nil, fmt.Errorf("unmarshalling excluded_columns: %w", err)
			}
		}

		configs = append(configs, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating selective sync configs: %w", err)
	}
	return configs, nil
}
