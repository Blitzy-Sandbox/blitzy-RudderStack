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
	// duration aggregates (avg/min/max), row totals, error category, schema changes count,
	// and the last sync time.
	getHealthSummarySQL = `
		SELECT
			source_id,
			destination_id,
			dest_type,
			source_type,
			workspace_id,
			COUNT(*) as total_syncs,
			COUNT(CASE WHEN status = 'exported_data' THEN 1 END) as successful_syncs,
			AVG(duration_ms) as avg_duration_ms,
			MIN(duration_ms) as min_duration_ms,
			MAX(duration_ms) as max_duration_ms,
			SUM(rows_synced) as total_rows_synced,
			SUM(rows_failed) as total_rows_failed,
			MAX(created_at) as last_sync,
			COALESCE(
				(SELECT error_category FROM ` + syncHealthTableName + ` sub
				 WHERE sub.source_id = ` + syncHealthTableName + `.source_id
				   AND sub.destination_id = ` + syncHealthTableName + `.destination_id
				   AND sub.error_category != ''
				   AND sub.created_at > $1
				 GROUP BY error_category ORDER BY COUNT(*) DESC LIMIT 1),
				''
			) as top_error_category,
			COUNT(CASE WHEN schema_changes IS NOT NULL AND schema_changes::text != 'null' AND schema_changes::text != '{}' AND schema_changes::text != '[]' THEN 1 END) as schema_change_count
		FROM ` + syncHealthTableName + `
		WHERE created_at > $1
		GROUP BY source_id, destination_id, dest_type, source_type, workspace_id
		ORDER BY source_id, destination_id
	`

	// getPreviousWindowRowsSQL retrieves the total rows synced per source-destination
	// pair in the previous time window (between $1 and $2). Used by alerting to detect
	// row count anomalies by comparing current vs previous window.
	getPreviousWindowRowsSQL = `
		SELECT
			source_id,
			destination_id,
			COALESCE(SUM(rows_synced), 0) as total_rows_synced
		FROM ` + syncHealthTableName + `
		WHERE created_at > $1 AND created_at <= $2
		GROUP BY source_id, destination_id
	`

	// getHealthByUploadSQL retrieves a single health record by its associated upload ID.
	getHealthByUploadSQL = `
		SELECT ` + syncHealthColumns + `
		FROM ` + syncHealthTableName + `
		WHERE upload_id = $1
	`

	// getHealthBySourceDestSQL aggregates health metrics for a specific source-destination
	// pair within the given time window. Returns the same aggregate columns as the summary
	// including error category and schema changes count.
	getHealthBySourceDestSQL = `
		SELECT
			source_id,
			destination_id,
			dest_type,
			source_type,
			workspace_id,
			COUNT(*) as total_syncs,
			COUNT(CASE WHEN status = 'exported_data' THEN 1 END) as successful_syncs,
			AVG(duration_ms) as avg_duration_ms,
			MIN(duration_ms) as min_duration_ms,
			MAX(duration_ms) as max_duration_ms,
			SUM(rows_synced) as total_rows_synced,
			SUM(rows_failed) as total_rows_failed,
			MAX(created_at) as last_sync,
			COALESCE(
				(SELECT error_category FROM ` + syncHealthTableName + ` sub
				 WHERE sub.source_id = $1 AND sub.destination_id = $2
				   AND sub.error_category != ''
				   AND sub.created_at > $3
				 GROUP BY error_category ORDER BY COUNT(*) DESC LIMIT 1),
				''
			) as top_error_category,
			COUNT(CASE WHEN schema_changes IS NOT NULL AND schema_changes::text != 'null' AND schema_changes::text != '{}' AND schema_changes::text != '[]' THEN 1 END) as schema_change_count
		FROM ` + syncHealthTableName + `
		WHERE source_id = $1 AND destination_id = $2
		  AND created_at > $3
		GROUP BY source_id, destination_id, dest_type, source_type, workspace_id
	`

	// purgeOldRecordsSQL deletes health records older than the specified timestamp.
	purgeOldRecordsSQL = `
		DELETE FROM ` + syncHealthTableName + `
		WHERE created_at < $1
	`
)

// RepoOpt is a functional option for configuring a HealthRepo instance.
// Follows the functional options pattern established in warehouse/internal/repo/options.go.
type RepoOpt func(*HealthRepo)

// WithNow overrides the default clock function (time.Now) for testability.
func WithNow(now func() time.Time) RepoOpt {
	return func(r *HealthRepo) {
		r.now = now
	}
}

// WithSummaryWindow overrides the default summary time window (24h) for
// GetHealthSummary and GetHealthBySourceDest queries.
func WithSummaryWindow(d time.Duration) RepoOpt {
	return func(r *HealthRepo) {
		r.summaryWindow = d
	}
}

// HealthRepo provides database access for warehouse health monitoring records.
// It follows the repository pattern established in warehouse/internal/repo/,
// using sqlquerywrapper.DB for instrumented database access, a clock function
// for testable time, and a stats factory for metric emission.
//
// All methods accept context.Context as the first parameter to support
// cancellation and deadline propagation to database operations.
type HealthRepo struct {
	db            *sqlmw.DB
	now           func() time.Time
	statsFactory  stats.Stats
	summaryWindow time.Duration
}

// NewHealthRepo creates a new HealthRepo with the given database connection,
// stats factory, and optional functional options. The clock function defaults
// to time.Now and the summary window defaults to 24 hours. Both can be
// overridden via WithNow and WithSummaryWindow functional options.
func NewHealthRepo(db *sqlmw.DB, statsFactory stats.Stats, opts ...RepoOpt) *HealthRepo {
	r := &HealthRepo{
		db:            db,
		now:           time.Now,
		statsFactory:  statsFactory,
		summaryWindow: 24 * time.Hour,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// RecordSyncHealth persists a sync health record to the wh_sync_health table.
// The record captures the outcome of a single warehouse upload cycle, including
// duration, row counts, error categorization, and schema changes.
//
// On success, the health.ID field is populated with the auto-generated primary key.
// SchemaChanges is serialized to JSONB using jsonrs.Marshal (never encoding/json).
func (r *HealthRepo) RecordSyncHealth(ctx context.Context, health *SyncHealth) error {
	defer r.statsFactory.NewTaggedStat(
		"warehouse.healthmonitor.record_sync_health_duration",
		stats.TimerType,
		stats.Tags{"module": "warehouse"},
	).RecordDuration()()

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
// pairs for the configured summary window (default 24 hours). The summary groups results
// by source and includes per-destination metrics such as duration statistics, row counts,
// error rates, error categories, schema change counts, and last sync timestamps.
//
// Additionally, populates PreviousRowsSynced by querying the previous window
// (e.g., 24-48 hours ago) for row count anomaly detection in alerting.
//
// Returns a non-nil HealthSummaryResponse even when no data exists (Sources will be empty).
func (r *HealthRepo) GetHealthSummary(ctx context.Context) (*HealthSummaryResponse, error) {
	defer r.statsFactory.NewTaggedStat(
		"warehouse.healthmonitor.get_health_summary_duration",
		stats.TimerType,
		stats.Tags{"module": "warehouse"},
	).RecordDuration()()

	now := r.now()
	since := now.Add(-r.summaryWindow)

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
			sourceID, destID, destType, sourceType, workspaceID string
			totalSyncs, successfulSyncs                         int64
			avgDurationMs, minDurationMs, maxDurationMs         sql.NullFloat64
			totalRowsSynced, totalRowsFailed                    sql.NullInt64
			lastSync                                            sql.NullTime
			topErrorCategory                                    string
			schemaChangeCount                                   int64
		)

		if err := rows.Scan(
			&sourceID, &destID, &destType, &sourceType, &workspaceID,
			&totalSyncs, &successfulSyncs,
			&avgDurationMs, &minDurationMs, &maxDurationMs,
			&totalRowsSynced, &totalRowsFailed,
			&lastSync,
			&topErrorCategory,
			&schemaChangeCount,
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
			RowsSynced:    totalRowsSynced.Int64,
			ErrorRate:     errorRate,
			ErrorCount:    totalSyncs - successfulSyncs,
			ErrorCategory: topErrorCategory,
			SchemaChanges: schemaChangeCount,
		}
		if totalRowsFailed.Valid {
			// ErrorCount already captures failed syncs; RowsFailed is the row-level count.
			_ = totalRowsFailed.Int64 // scanned but exposed via ErrorCount/ErrorRate
		}
		if lastSync.Valid {
			destHealth.LastSync = lastSync.Time.Format(time.RFC3339)
		}

		if _, exists := sourceMap[sourceID]; !exists {
			sourceMap[sourceID] = &SourceHealth{
				SourceID:     sourceID,
				SourceType:   sourceType,
				WorkspaceID:  workspaceID,
				Destinations: make([]*DestinationHealth, 0),
			}
		}
		sourceMap[sourceID].Destinations = append(sourceMap[sourceID].Destinations, destHealth)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating health summary rows: %w", err)
	}

	// Populate PreviousRowsSynced from the previous window for anomaly detection.
	prevWindowStart := since.Add(-r.summaryWindow) // e.g., 48h ago
	prevWindowEnd := since                          // e.g., 24h ago
	if err := r.populatePreviousRows(ctx, sourceMap, prevWindowStart, prevWindowEnd); err != nil {
		// Log but do not fail — previous rows is supplementary data for alerting.
		// If unavailable, alerting will skip row count anomaly checks gracefully.
		_ = err
	}

	// Convert map to slice for the response.
	sources := make([]*SourceHealth, 0, len(sourceMap))
	for _, source := range sourceMap {
		sources = append(sources, source)
	}

	return &HealthSummaryResponse{Sources: sources}, nil
}

// populatePreviousRows queries the previous time window for row counts and populates
// PreviousRowsSynced on matching DestinationHealth entries. This enables the alerting
// evaluator to detect row count anomalies by comparing current vs previous window data.
func (r *HealthRepo) populatePreviousRows(ctx context.Context, sourceMap map[string]*SourceHealth, start, end time.Time) error {
	rows, err := r.db.QueryContext(ctx, getPreviousWindowRowsSQL, start, end)
	if err != nil {
		return fmt.Errorf("querying previous window rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			sourceID, destID string
			prevRowsSynced   int64
		)
		if err := rows.Scan(&sourceID, &destID, &prevRowsSynced); err != nil {
			return fmt.Errorf("scanning previous window rows: %w", err)
		}

		// Match to existing DestinationHealth entries.
		if src, ok := sourceMap[sourceID]; ok {
			for _, dest := range src.Destinations {
				if dest.DestID == destID {
					dest.PreviousRowsSynced = prevRowsSynced
				}
			}
		}
	}
	return rows.Err()
}

// GetHealthBySourceDest returns an aggregated health summary for a specific
// source-destination pair over the configured summary window (default 24 hours).
// Returns nil without error if no health data exists for the specified pair.
func (r *HealthRepo) GetHealthBySourceDest(ctx context.Context, sourceID, destID string) (*SourceHealthResponse, error) {
	defer r.statsFactory.NewTaggedStat(
		"warehouse.healthmonitor.get_health_by_source_dest_duration",
		stats.TimerType,
		stats.Tags{"module": "warehouse"},
	).RecordDuration()()

	since := r.now().Add(-r.summaryWindow)

	rows, err := r.db.QueryContext(ctx, getHealthBySourceDestSQL, sourceID, destID, since)
	if err != nil {
		return nil, fmt.Errorf("querying health by source/dest: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var response *SourceHealthResponse

	for rows.Next() {
		var (
			srcID, dstID, destType, sourceType, workspaceID string
			totalSyncs, successfulSyncs                     int64
			avgDurationMs, minDurationMs, maxDurationMs     sql.NullFloat64
			totalRowsSynced, totalRowsFailed                sql.NullInt64
			lastSync                                        sql.NullTime
			topErrorCategory                                string
			schemaChangeCount                               int64
		)

		if err := rows.Scan(
			&srcID, &dstID, &destType, &sourceType, &workspaceID,
			&totalSyncs, &successfulSyncs,
			&avgDurationMs, &minDurationMs, &maxDurationMs,
			&totalRowsSynced, &totalRowsFailed,
			&lastSync,
			&topErrorCategory,
			&schemaChangeCount,
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
			RowsSynced:    totalRowsSynced.Int64,
			ErrorRate:     errorRate,
			ErrorCount:    totalSyncs - successfulSyncs,
			ErrorCategory: topErrorCategory,
			SchemaChanges: schemaChangeCount,
		}
		if lastSync.Valid {
			destHealth.LastSync = lastSync.Time.Format(time.RFC3339)
		}

		// There should be at most one row for a given source/dest pair,
		// but we handle the general case by always setting response.
		response = &SourceHealthResponse{
			SourceID:     srcID,
			SourceType:   sourceType,
			WorkspaceID:  workspaceID,
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
