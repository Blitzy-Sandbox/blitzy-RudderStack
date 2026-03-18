package healthmonitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/stats"

	sqlmw "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
)

const (
	// syncHealthTableName is the database table name for health monitoring records.
	syncHealthTableName = "wh_sync_health"

	// syncHealthColumns lists all columns in the wh_sync_health table,
	// ordered to match the column sequence used in INSERT and SELECT statements.
	syncHealthColumns = `
		id,
		upload_id,
		source_id,
		destination_id,
		dest_type,
		source_type,
		workspace_id,
		status,
		duration_ms,
		rows_synced,
		rows_failed,
		error_category,
		schema_changes,
		created_at
	`

	// insertSyncHealthSQL inserts a new health record and returns its auto-generated ID.
	insertSyncHealthSQL = `
		INSERT INTO ` + syncHealthTableName + ` (
			upload_id, source_id, destination_id, dest_type, source_type,
			workspace_id, status, duration_ms, rows_synced, rows_failed,
			error_category, schema_changes, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id
	`

	// getHealthSummarySQL aggregates health metrics per source-destination pair
	// for the specified time window. Returns total sync counts, success counts,
	// duration aggregates (avg/min/max), row totals, and the last sync time.
	getHealthSummarySQL = `
		SELECT
			source_id,
			destination_id,
			dest_type,
			source_type,
			COUNT(*) as total_syncs,
			COUNT(CASE WHEN status = 'exported_data' THEN 1 END) as successful_syncs,
			AVG(duration_ms) as avg_duration_ms,
			MIN(duration_ms) as min_duration_ms,
			MAX(duration_ms) as max_duration_ms,
			SUM(rows_synced) as total_rows_synced,
			SUM(rows_failed) as total_rows_failed,
			MAX(created_at) as last_sync
		FROM ` + syncHealthTableName + `
		WHERE created_at > $1
		GROUP BY source_id, destination_id, dest_type, source_type
		ORDER BY source_id, destination_id
	`

	// getHealthByUploadSQL retrieves a single health record by its associated upload ID.
	getHealthByUploadSQL = `
		SELECT ` + syncHealthColumns + `
		FROM ` + syncHealthTableName + `
		WHERE upload_id = $1
	`

	// getHealthBySourceDestSQL aggregates health metrics for a specific source-destination
	// pair within the given time window. Returns the same aggregate columns as the summary.
	getHealthBySourceDestSQL = `
		SELECT
			source_id,
			destination_id,
			dest_type,
			source_type,
			COUNT(*) as total_syncs,
			COUNT(CASE WHEN status = 'exported_data' THEN 1 END) as successful_syncs,
			AVG(duration_ms) as avg_duration_ms,
			MIN(duration_ms) as min_duration_ms,
			MAX(duration_ms) as max_duration_ms,
			SUM(rows_synced) as total_rows_synced,
			SUM(rows_failed) as total_rows_failed,
			MAX(created_at) as last_sync
		FROM ` + syncHealthTableName + `
		WHERE source_id = $1 AND destination_id = $2
		  AND created_at > $3
		GROUP BY source_id, destination_id, dest_type, source_type
	`

	// purgeOldRecordsSQL deletes health records older than the specified timestamp.
	purgeOldRecordsSQL = `
		DELETE FROM ` + syncHealthTableName + `
		WHERE created_at < $1
	`
)

// HealthRepo provides database access for warehouse health monitoring records.
// It follows the repository pattern established in warehouse/internal/repo/,
// using sqlquerywrapper.DB for instrumented database access, a clock function
// for testable time, and a stats factory for metric emission.
//
// All methods accept context.Context as the first parameter to support
// cancellation and deadline propagation to database operations.
type HealthRepo struct {
	db           *sqlmw.DB
	now          func() time.Time
	statsFactory stats.Stats
}

// NewHealthRepo creates a new HealthRepo with the given database connection and
// stats factory. The clock function defaults to time.Now and can be overridden
// for testing via direct struct field assignment.
func NewHealthRepo(db *sqlmw.DB, statsFactory stats.Stats) *HealthRepo {
	return &HealthRepo{
		db:           db,
		now:          time.Now,
		statsFactory: statsFactory,
	}
}

// RecordSyncHealth persists a sync health record to the wh_sync_health table.
// The record captures the outcome of a single warehouse upload cycle, including
// duration, row counts, error categorization, and schema changes.
//
// On success, the health.ID field is populated with the auto-generated primary key.
// SchemaChanges is serialized to JSONB using jsonrs.Marshal (never encoding/json).
func (r *HealthRepo) RecordSyncHealth(ctx context.Context, health *SyncHealth) error {
	schemaChangesJSON, err := jsonrs.Marshal(health.SchemaChanges)
	if err != nil {
		return fmt.Errorf("marshaling schema changes: %w", err)
	}

	var id int64
	err = r.db.QueryRowContext(ctx, insertSyncHealthSQL,
		health.UploadID,
		health.SourceID,
		health.DestinationID,
		health.DestType,
		health.SourceType,
		health.WorkspaceID,
		health.Status,
		health.DurationMs,
		health.RowsSynced,
		health.RowsFailed,
		health.ErrorCategory,
		schemaChangesJSON,
		r.now(),
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("inserting sync health record: %w", err)
	}

	health.ID = id
	return nil
}

// GetHealthSummary returns an aggregated health summary across all source/destination
// pairs for the last 24 hours. The summary groups results by source and includes
// per-destination metrics such as duration statistics, row counts, error rates,
// and last sync timestamps.
//
// Returns a non-nil HealthSummaryResponse even when no data exists (Sources will be empty).
func (r *HealthRepo) GetHealthSummary(ctx context.Context) (*HealthSummaryResponse, error) {
	since := r.now().Add(-24 * time.Hour)

	rows, err := r.db.QueryContext(ctx, getHealthSummarySQL, since)
	if err != nil {
		return nil, fmt.Errorf("querying health summary: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Build the response by aggregating per sourceID.
	// A map preserves the grouping while allowing efficient lookups.
	sourceMap := make(map[string]*SourceHealth)

	for rows.Next() {
		var (
			sourceID, destID, destType, sourceType       string
			totalSyncs, successfulSyncs                  int64
			avgDurationMs, minDurationMs, maxDurationMs  sql.NullFloat64
			totalRowsSynced, totalRowsFailed              sql.NullInt64
			lastSync                                     sql.NullTime
		)

		if err := rows.Scan(
			&sourceID, &destID, &destType, &sourceType,
			&totalSyncs, &successfulSyncs,
			&avgDurationMs, &minDurationMs, &maxDurationMs,
			&totalRowsSynced, &totalRowsFailed,
			&lastSync,
		); err != nil {
			return nil, fmt.Errorf("scanning health summary row: %w", err)
		}

		// Calculate error rate as the ratio of failed syncs to total syncs.
		var errorRate float64
		if totalSyncs > 0 {
			errorRate = 1.0 - float64(successfulSyncs)/float64(totalSyncs)
		}

		destHealth := &DestinationHealth{
			DestID:   destID,
			DestType: destType,
			SyncDuration: DurationStats{
				Min: int64(minDurationMs.Float64),
				Max: int64(maxDurationMs.Float64),
				Avg: int64(avgDurationMs.Float64),
			},
			RowsSynced: totalRowsSynced.Int64,
			ErrorRate:  errorRate,
			ErrorCount: totalSyncs - successfulSyncs,
		}
		if lastSync.Valid {
			destHealth.LastSync = lastSync.Time.Format(time.RFC3339)
		}

		if _, exists := sourceMap[sourceID]; !exists {
			sourceMap[sourceID] = &SourceHealth{
				SourceID:     sourceID,
				SourceType:   sourceType,
				Destinations: make([]*DestinationHealth, 0),
			}
		}
		sourceMap[sourceID].Destinations = append(sourceMap[sourceID].Destinations, destHealth)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating health summary rows: %w", err)
	}

	// Convert map to slice for the response.
	sources := make([]*SourceHealth, 0, len(sourceMap))
	for _, source := range sourceMap {
		sources = append(sources, source)
	}

	return &HealthSummaryResponse{Sources: sources}, nil
}

// GetHealthBySourceDest returns an aggregated health summary for a specific
// source-destination pair over the last 24 hours. Returns nil without error
// if no health data exists for the specified pair.
func (r *HealthRepo) GetHealthBySourceDest(ctx context.Context, sourceID, destID string) (*SourceHealthResponse, error) {
	since := r.now().Add(-24 * time.Hour)

	rows, err := r.db.QueryContext(ctx, getHealthBySourceDestSQL, sourceID, destID, since)
	if err != nil {
		return nil, fmt.Errorf("querying health by source/dest: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var response *SourceHealthResponse

	for rows.Next() {
		var (
			srcID, dstID, destType, sourceType           string
			totalSyncs, successfulSyncs                  int64
			avgDurationMs, minDurationMs, maxDurationMs  sql.NullFloat64
			totalRowsSynced, totalRowsFailed              sql.NullInt64
			lastSync                                     sql.NullTime
		)

		if err := rows.Scan(
			&srcID, &dstID, &destType, &sourceType,
			&totalSyncs, &successfulSyncs,
			&avgDurationMs, &minDurationMs, &maxDurationMs,
			&totalRowsSynced, &totalRowsFailed,
			&lastSync,
		); err != nil {
			return nil, fmt.Errorf("scanning health by source/dest row: %w", err)
		}

		// Calculate error rate as the ratio of failed syncs to total syncs.
		var errorRate float64
		if totalSyncs > 0 {
			errorRate = 1.0 - float64(successfulSyncs)/float64(totalSyncs)
		}

		destHealth := &DestinationHealth{
			DestID:   dstID,
			DestType: destType,
			SyncDuration: DurationStats{
				Min: int64(minDurationMs.Float64),
				Max: int64(maxDurationMs.Float64),
				Avg: int64(avgDurationMs.Float64),
			},
			RowsSynced: totalRowsSynced.Int64,
			ErrorRate:  errorRate,
			ErrorCount: totalSyncs - successfulSyncs,
		}
		if lastSync.Valid {
			destHealth.LastSync = lastSync.Time.Format(time.RFC3339)
		}

		// There should be at most one row for a given source/dest pair,
		// but we handle the general case by always setting response.
		response = &SourceHealthResponse{
			SourceID:     srcID,
			SourceType:   sourceType,
			Destinations: []*DestinationHealth{destHealth},
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating health by source/dest rows: %w", err)
	}

	return response, nil
}

// GetHealthByUpload retrieves a single sync health record associated with the
// given upload ID. Returns ErrHealthNotFound if no record exists for the upload.
//
// SchemaChanges is deserialized from JSONB using jsonrs.Unmarshal (never encoding/json).
func (r *HealthRepo) GetHealthByUpload(ctx context.Context, uploadID int64) (*SyncHealth, error) {
	row := r.db.QueryRowContext(ctx, getHealthByUploadSQL, uploadID)

	var (
		health           SyncHealth
		schemaChangesRaw []byte
		createdAt        sql.NullTime
	)

	err := row.Scan(
		&health.ID,
		&health.UploadID,
		&health.SourceID,
		&health.DestinationID,
		&health.DestType,
		&health.SourceType,
		&health.WorkspaceID,
		&health.Status,
		&health.DurationMs,
		&health.RowsSynced,
		&health.RowsFailed,
		&health.ErrorCategory,
		&schemaChangesRaw,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrHealthNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning health record by upload ID %d: %w", uploadID, err)
	}

	// Deserialize schema_changes JSONB field using jsonrs (never encoding/json).
	if len(schemaChangesRaw) > 0 {
		if err := jsonrs.Unmarshal(schemaChangesRaw, &health.SchemaChanges); err != nil {
			return nil, fmt.Errorf("unmarshaling schema changes for upload %d: %w", uploadID, err)
		}
	}

	if createdAt.Valid {
		health.CreatedAt = createdAt.Time.UTC()
	}

	return &health, nil
}

// PurgeOldRecords deletes health records with created_at before the specified timestamp.
// Returns the number of rows deleted. This is called periodically by the HealthMonitor
// to enforce the configured retention window (default: 30 days).
func (r *HealthRepo) PurgeOldRecords(ctx context.Context, before time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx, purgeOldRecordsSQL, before)
	if err != nil {
		return 0, fmt.Errorf("purging old health records: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("getting rows affected after purge: %w", err)
	}
	return count, nil
}
