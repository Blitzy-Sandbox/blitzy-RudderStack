package backfill

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

const (
	// backfillJobsTableName is the database table name for persisting backfill job records.
	backfillJobsTableName = "wh_backfill_jobs"

	// backfillJobColumns lists all columns in wh_backfill_jobs for SELECT queries.
	// Order must match the Scan() call sites in Get and ListBySource.
	backfillJobColumns = `id, source_id, destination_id, workspace_id, start_date, end_date, status, metadata, created_at, updated_at`
)

// rawJSON wraps a []byte so that jsonrs.Marshal produces the raw JSON bytes
// instead of a base64-encoded string. Without this wrapper, jsonrs.Marshal([]byte)
// treats the data as an opaque byte slice and base64-encodes it, which is incorrect
// for JSONB column storage where the database expects raw JSON text.
type rawJSON []byte

// MarshalJSON implements the json.Marshaler interface, returning the raw bytes
// as-is (they are already valid JSON) or "null" when the slice is empty/nil.
func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// Repository provides CRUD operations for the wh_backfill_jobs table.
// It follows the repository pattern established in warehouse/internal/repo/,
// using sqlquerywrapper.DB for query instrumentation and functional options
// for testability (injectable clock and stats).
type Repository struct {
	db           *sqlmiddleware.DB
	now          func() time.Time
	statsFactory stats.Stats
}

// RepositoryOption is a functional option for configuring a Repository instance.
type RepositoryOption func(*Repository)

// NewRepository creates a new Repository with the given database handle and options.
// Defaults: clock is timeutil.Now (UTC), stats is stats.NOP (no-op metrics).
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

// WithNow returns a RepositoryOption that overrides the time source used for
// populating created_at and updated_at timestamps. Primarily used in tests
// to inject a deterministic clock.
func WithNow(now func() time.Time) RepositoryOption {
	return func(r *Repository) {
		r.now = now
	}
}

// WithStats returns a RepositoryOption that overrides the stats factory used for
// recording per-query duration metrics. When set, every CRUD method emits a
// timer stat with the name pattern:
//
//	warehouse_repo_query_backfill_jobs_<action>_duration_seconds
func WithStats(s stats.Stats) RepositoryOption {
	return func(r *Repository) {
		r.statsFactory = s
	}
}

// timerStat returns a deferred stop function that, when called, records the elapsed
// duration of a database operation as a Prometheus-compatible timer metric.
// Usage: defer r.timerStat("create", tags)()
//
// The metric name follows the convention from warehouse/internal/repo/repo.go:
//
//	warehouse_repo_query_backfill_jobs_<action>_duration_seconds
func (r *Repository) timerStat(action string, extraTags stats.Tags) func() {
	tags := stats.Tags{}
	for k, v := range extraTags {
		tags[k] = v
	}
	return r.statsFactory.NewTaggedStat(
		"warehouse_repo_query_backfill_jobs_"+action+"_duration_seconds",
		stats.TimerType,
		tags,
	).RecordDuration()
}

// Create inserts a new backfill job into the wh_backfill_jobs table and returns
// the auto-generated ID. The job's status is always set to StatusPending regardless
// of what the caller provides, ensuring consistent state machine entry.
//
// The metadata field (arbitrary JSON) is serialized through jsonrs.Marshal using the
// rawJSON wrapper to preserve raw JSON content in the JSONB column without base64 encoding.
func (r *Repository) Create(ctx context.Context, job BackfillJob) (int64, error) {
	defer r.timerStat("create", stats.Tags{
		"sourceId":      job.SourceID,
		"destinationId": job.DestinationID,
		"workspaceId":   job.WorkspaceID,
	})()

	// Serialize metadata using jsonrs (mandated JSON package per .golangci.yml depguard rule).
	// The rawJSON wrapper ensures raw JSON bytes pass through without base64 encoding.
	var metadataBytes []byte
	if job.Metadata != nil {
		var err error
		metadataBytes, err = jsonrs.Marshal(rawJSON(job.Metadata))
		if err != nil {
			return 0, fmt.Errorf("marshalling backfill job metadata: %w", err)
		}
	}

	now := r.now()
	var id int64
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO `+backfillJobsTableName+` (
			source_id, destination_id, workspace_id,
			start_date, end_date, status, metadata,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		job.SourceID, job.DestinationID, job.WorkspaceID,
		job.StartDate, job.EndDate, StatusPending,
		metadataBytes, now, now,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("creating backfill job: %w", err)
	}
	return id, nil
}

// Get retrieves a single backfill job by its primary key ID.
// Returns ErrBackfillJobNotFound if no row exists with the given ID.
func (r *Repository) Get(ctx context.Context, id int64) (BackfillJob, error) {
	defer r.timerStat("get", nil)()

	var job BackfillJob
	var metadata sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT `+backfillJobColumns+`
		FROM `+backfillJobsTableName+`
		WHERE id = $1`,
		id,
	).Scan(
		&job.ID, &job.SourceID, &job.DestinationID, &job.WorkspaceID,
		&job.StartDate, &job.EndDate, &job.Status, &metadata,
		&job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BackfillJob{}, ErrBackfillJobNotFound
		}
		return BackfillJob{}, fmt.Errorf("getting backfill job %d: %w", id, err)
	}
	if metadata.Valid {
		job.Metadata = []byte(metadata.String)
	}
	return job, nil
}

// UpdateStatus updates the status column and updated_at timestamp for the
// backfill job identified by the given ID.
// Returns ErrBackfillJobNotFound if no row exists with the given ID.
func (r *Repository) UpdateStatus(ctx context.Context, id int64, status string) error {
	defer r.timerStat("update_status", nil)()

	result, err := r.db.ExecContext(ctx,
		`UPDATE `+backfillJobsTableName+`
		SET status = $1, updated_at = $2
		WHERE id = $3`,
		status, r.now(), id,
	)
	if err != nil {
		return fmt.Errorf("updating backfill job %d status to %q: %w", id, status, err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected for backfill job %d: %w", id, err)
	}
	if rowsAffected == 0 {
		return ErrBackfillJobNotFound
	}
	return nil
}

// ListBySource returns all backfill jobs for the given source ID, ordered by
// created_at descending (most recent first). Returns an empty (non-nil) slice
// if no jobs exist for the source.
func (r *Repository) ListBySource(ctx context.Context, sourceID string) ([]BackfillJob, error) {
	defer r.timerStat("list_by_source", stats.Tags{"sourceId": sourceID})()

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+backfillJobColumns+`
		FROM `+backfillJobsTableName+`
		WHERE source_id = $1
		ORDER BY created_at DESC
		LIMIT 100`,
		sourceID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing backfill jobs for source %q: %w", sourceID, err)
	}
	defer func() { _ = rows.Close() }()

	jobs := make([]BackfillJob, 0)
	for rows.Next() {
		var job BackfillJob
		var metadata sql.NullString
		if err := rows.Scan(
			&job.ID, &job.SourceID, &job.DestinationID, &job.WorkspaceID,
			&job.StartDate, &job.EndDate, &job.Status, &metadata,
			&job.CreatedAt, &job.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning backfill job row: %w", err)
		}
		if metadata.Valid {
			job.Metadata = []byte(metadata.String)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating backfill job rows: %w", err)
	}
	return jobs, nil
}

// GetActiveCount returns the number of backfill jobs that are currently in an
// active state (StatusPending or StatusInProgress). This is used by the backfill
// service to enforce concurrency limits on concurrent backfill operations.
func (r *Repository) GetActiveCount(ctx context.Context) (int64, error) {
	defer r.timerStat("get_active_count", nil)()

	var count int64
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		FROM `+backfillJobsTableName+`
		WHERE status IN ($1, $2)`,
		StatusPending, StatusInProgress,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("getting active backfill job count: %w", err)
	}
	return count, nil
}

// CreateIfUnderLimit atomically checks that the number of active (Pending or InProgress)
// backfill jobs is below maxConcurrent, and inserts a new job only if the limit is
// not reached. This eliminates the TOCTOU race condition that exists when
// GetActiveCount and Create are called as separate operations.
//
// The implementation uses a CTE (Common Table Expression) to perform the count
// check and conditional insert in a single SQL statement, providing atomicity
// at the database level without requiring explicit advisory locks.
//
// Returns:
//   - (id, nil) if the job was successfully created under the limit
//   - (0, ErrConcurrentLimitReached) if the active job count >= maxConcurrent
//   - (0, error) for any other database error
func (r *Repository) CreateIfUnderLimit(ctx context.Context, job BackfillJob, maxConcurrent int) (int64, error) {
	defer r.timerStat("create_if_under_limit", stats.Tags{
		"sourceId":      job.SourceID,
		"destinationId": job.DestinationID,
		"workspaceId":   job.WorkspaceID,
	})()

	// Serialize metadata using jsonrs (mandated JSON package per .golangci.yml depguard rule).
	var metadataBytes []byte
	if job.Metadata != nil {
		var err error
		metadataBytes, err = jsonrs.Marshal(rawJSON(job.Metadata))
		if err != nil {
			return 0, fmt.Errorf("marshalling backfill job metadata: %w", err)
		}
	}

	now := r.now()

	// Use an explicit transaction with a PostgreSQL advisory lock to serialise
	// concurrent backfill-creation attempts. The advisory lock (released
	// automatically on COMMIT/ROLLBACK) guarantees that only one transaction
	// at a time can evaluate the active-job count and insert, eliminating the
	// TOCTOU race that arises when multiple requests check the count under
	// READ COMMITTED isolation before any of them commit their inserts.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction for backfill creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// Acquire a transaction-scoped advisory lock. The constant 'backfill_concurrent_limit'
	// is hashed by hashtext() to produce a stable int4 key. All concurrent callers
	// block here until the holding transaction completes.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('backfill_concurrent_limit'))`); err != nil {
		return 0, fmt.Errorf("acquiring advisory lock for backfill creation: %w", err)
	}

	var id int64
	// Count active jobs and insert in the same serialised transaction.
	// If the count >= limit, the SELECT yields zero rows, the INSERT inserts
	// nothing, and QueryRowContext returns sql.ErrNoRows — mapped to
	// ErrConcurrentLimitReached.
	err = tx.QueryRowContext(ctx,
		`WITH active_check AS (
			SELECT COUNT(*) AS cnt
			FROM `+backfillJobsTableName+`
			WHERE status IN ($1, $2)
		)
		INSERT INTO `+backfillJobsTableName+` (
			source_id, destination_id, workspace_id,
			start_date, end_date, status, metadata,
			created_at, updated_at
		)
		SELECT $3, $4, $5, $6, $7, $8, $9, $10, $11
		FROM active_check
		WHERE active_check.cnt < $12
		RETURNING id`,
		StatusPending, StatusInProgress,
		job.SourceID, job.DestinationID, job.WorkspaceID,
		job.StartDate, job.EndDate, StatusPending,
		metadataBytes, now, now,
		maxConcurrent,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrConcurrentLimitReached
		}
		return 0, fmt.Errorf("creating backfill job with limit check: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing backfill job creation: %w", err)
	}
	return id, nil
}
