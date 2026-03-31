// Package storage provides PostgreSQL persistence for the identity graph.
//
// This package implements the Repository interface for persisting identity
// graph data (segments, external IDs, traits) in PostgreSQL. It is the
// foundational persistence layer consumed by identity/graph/, identity/profiles/,
// and identity/sync/ packages as part of the Identity Resolution feature (E-026).
//
// All public methods accept context.Context as first parameter for cancellation
// and timeout propagation. Bulk operations use PostgreSQL COPY protocol via
// pq.CopyIn for high-throughput loading.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// Table name constants matching the migration schema in sql/migrations/identity/.
const (
	tableIdentityGraph       = "identity_graph"
	tableIdentityExternalIDs = "identity_external_ids"
	tableIdentityTraits      = "identity_traits"
)

var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("identity").Child("storage")
}

// Repository defines the persistence contract for the identity graph.
// Implementations must be safe for concurrent use.
type Repository interface {
	// Graph segment operations
	CreateSegment(ctx context.Context, workspaceID string) (int64, error)
	GetSegment(ctx context.Context, id int64) (*GraphSegment, error)
	GetSegmentByWorkspace(ctx context.Context, workspaceID string, limit, offset int) ([]GraphSegment, error)
	MergeSegments(ctx context.Context, targetSegmentID int64, sourceSegmentIDs []int64) error

	// External ID operations
	AddExternalID(ctx context.Context, externalID ExternalID) (int64, error)
	GetExternalIDsBySegment(ctx context.Context, segmentID int64) ([]ExternalID, error)
	LookupByExternalID(ctx context.Context, workspaceID, externalIDType, externalIDValue string) (*GraphSegment, error)
	BulkAddExternalIDs(ctx context.Context, externalIDs []ExternalID) error

	// Trait operations
	SetTrait(ctx context.Context, graphID int64, key, value string) error
	GetTraits(ctx context.Context, graphID int64) ([]Trait, error)
	BulkSetTraits(ctx context.Context, traits []Trait) error

	// Batch/profile assembly operations
	GetProfileData(ctx context.Context, segmentID int64) (*ProfileData, error)

	// Transaction support
	WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error

	// Health check
	Ping(ctx context.Context) error
}

// GraphSegment represents a node in the identity graph.
// Maps to the identity_graph table created in sql/migrations/identity/.
type GraphSegment struct {
	ID          int64     `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	SegmentID   string    `json:"segment_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// ExternalID represents an external identifier associated with an identity graph segment.
// Supports 12+ external identifier types (user_id, email, anonymous_id, ios.id, android.id, etc.).
// Maps to the identity_external_ids table.
type ExternalID struct {
	ID              int64      `json:"id"`
	GraphID         int64      `json:"graph_id"`
	ExternalIDType  string     `json:"external_id_type"`
	ExternalIDValue string     `json:"external_id_value"`
	CreatedSource   string     `json:"created_source"`
	CreatedAt       time.Time  `json:"created_at"`
	MergedAt        *time.Time `json:"merged_at,omitempty"`
	MergedFrom      *int64     `json:"merged_from,omitempty"`
}

// Trait represents a key-value attribute for a profile in the identity graph.
// Maps to the identity_traits table.
type Trait struct {
	ID        int64     `json:"id"`
	GraphID   int64     `json:"graph_id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProfileData combines graph segment, external IDs, and traits for a complete profile.
// Used by the Profiles API (E-027) for batch profile assembly.
type ProfileData struct {
	Segment     GraphSegment `json:"segment"`
	ExternalIDs []ExternalID `json:"external_ids"`
	Traits      []Trait      `json:"traits"`
}

// PostgresRepository implements Repository using PostgreSQL.
// It is safe for concurrent use since the underlying *sql.DB manages its own
// connection pool. All operations use parameterized queries for SQL injection safety.
//
// Design note: This package uses *sql.DB directly rather than warehouse/internal/repo's
// sqlmiddleware.DB. While the AAP references warehouse/identity/identity.go which uses
// sqlmiddleware.DB, that middleware is warehouse-specific (adding warehouse-scoped tracing
// and upload lifecycle hooks). The identity/ package is a new top-level service independent
// of the warehouse subsystem, following the warehouse/backfill/repository.go pattern of
// direct *sql.DB usage. Observability is provided through the logger and through
// instrumented database drivers (e.g., otelsql) configured at the connection level by
// the caller, avoiding a circular dependency on warehouse internals.
type PostgresRepository struct {
	db     *sql.DB
	logger logger.Logger
}

// NewPostgresRepository creates a new PostgresRepository with the given database connection.
// The *sql.DB is used directly (see PostgresRepository design note above).
// If log is nil, the package-level logger is used.
func NewPostgresRepository(db *sql.DB, log logger.Logger) *PostgresRepository {
	if log == nil {
		log = pkgLogger
	}
	return &PostgresRepository{
		db:     db,
		logger: log,
	}
}

// CreateSegment inserts a new row into the identity_graph table and returns its ID.
// A UUID-based segment_id is generated automatically for each new segment.
func (r *PostgresRepository) CreateSegment(ctx context.Context, workspaceID string) (int64, error) {
	segmentID := uuid.New().String()
	now := time.Now()

	sqlStmt := fmt.Sprintf(
		`INSERT INTO %s (workspace_id, segment_id, created_at) VALUES ($1, $2, $3) RETURNING id`,
		tableIdentityGraph,
	)

	var id int64
	err := r.db.QueryRowContext(ctx, sqlStmt, workspaceID, segmentID, now).Scan(&id)
	if err != nil {
		r.logger.Errorn("Error creating segment",
			logger.NewStringField("workspaceID", workspaceID),
			obskit.Error(err),
		)
		return 0, fmt.Errorf("creating segment: %w", err)
	}

	r.logger.Debugn("Created identity graph segment",
		logger.NewIntField("segmentID", id),
		logger.NewStringField("workspaceID", workspaceID),
	)
	return id, nil
}

// GetSegment retrieves a single graph segment by its primary key ID.
// Returns nil, nil if no segment is found (sql.ErrNoRows is suppressed).
func (r *PostgresRepository) GetSegment(ctx context.Context, id int64) (*GraphSegment, error) {
	sqlStmt := fmt.Sprintf(
		`SELECT id, workspace_id, segment_id, created_at FROM %s WHERE id = $1`,
		tableIdentityGraph,
	)

	var seg GraphSegment
	err := r.db.QueryRowContext(ctx, sqlStmt, id).Scan(
		&seg.ID, &seg.WorkspaceID, &seg.SegmentID, &seg.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		r.logger.Errorn("Error getting segment",
			logger.NewIntField("id", id),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("getting segment %d: %w", id, err)
	}
	return &seg, nil
}

// GetSegmentByWorkspace retrieves identity graph segments filtered by workspace ID
// with pagination support via limit and offset parameters.
// Results are ordered by created_at descending (newest first).
func (r *PostgresRepository) GetSegmentByWorkspace(ctx context.Context, workspaceID string, limit, offset int) ([]GraphSegment, error) {
	sqlStmt := fmt.Sprintf(
		`SELECT id, workspace_id, segment_id, created_at FROM %s WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		tableIdentityGraph,
	)

	rows, err := r.db.QueryContext(ctx, sqlStmt, workspaceID, limit, offset)
	if err != nil {
		r.logger.Errorn("Error querying segments by workspace",
			logger.NewStringField("workspaceID", workspaceID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("querying segments by workspace %s: %w", workspaceID, err)
	}
	defer func() { _ = rows.Close() }()

	var segments []GraphSegment
	for rows.Next() {
		var seg GraphSegment
		if err := rows.Scan(&seg.ID, &seg.WorkspaceID, &seg.SegmentID, &seg.CreatedAt); err != nil {
			r.logger.Errorn("Error scanning segment row",
				logger.NewStringField("workspaceID", workspaceID),
				obskit.Error(err),
			)
			return nil, fmt.Errorf("scanning segment row: %w", err)
		}
		segments = append(segments, seg)
	}
	if err := rows.Err(); err != nil {
		r.logger.Errorn("Error iterating segment rows",
			logger.NewStringField("workspaceID", workspaceID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("iterating segment rows: %w", err)
	}
	return segments, nil
}

// MergeSegments atomically merges source segments into a target segment within a transaction.
// It reassigns all external IDs and traits from source segments to the target segment,
// then deletes the source segments. This follows the transaction pattern from
// warehouse/identity/identity.go processMergeRules.
func (r *PostgresRepository) MergeSegments(ctx context.Context, targetSegmentID int64, sourceSegmentIDs []int64) error {
	if len(sourceSegmentIDs) == 0 {
		return nil
	}

	return r.WithTx(ctx, func(tx *sql.Tx) error {
		now := time.Now()

		// Step 1: Reassign external IDs from source segments to target segment.
		// Record the original graph_id in merged_from and set merged_at timestamp.
		updateExtIDsSQL := fmt.Sprintf(
			`UPDATE %s SET graph_id = $1, merged_at = $2, merged_from = %s.graph_id WHERE graph_id = ANY($3)`,
			tableIdentityExternalIDs, tableIdentityExternalIDs,
		)
		_, err := tx.ExecContext(ctx, updateExtIDsSQL, targetSegmentID, now, pq.Array(sourceSegmentIDs))
		if err != nil {
			r.logger.Errorn("Error reassigning external IDs during merge",
				logger.NewIntField("targetSegmentID", targetSegmentID),
				obskit.Error(err),
			)
			return fmt.Errorf("reassigning external IDs during merge: %w", err)
		}

		// Step 2: Upsert traits from source segments into target segment.
		// ON CONFLICT updates the value and timestamp for duplicate keys.
		upsertTraitsSQL := fmt.Sprintf(
			`INSERT INTO %s (graph_id, key, value, updated_at)
			 SELECT $1, key, value, $2 FROM %s WHERE graph_id = ANY($3)
			 ON CONFLICT (graph_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
			tableIdentityTraits, tableIdentityTraits,
		)
		_, err = tx.ExecContext(ctx, upsertTraitsSQL, targetSegmentID, now, pq.Array(sourceSegmentIDs))
		if err != nil {
			r.logger.Errorn("Error upserting traits during merge",
				logger.NewIntField("targetSegmentID", targetSegmentID),
				obskit.Error(err),
			)
			return fmt.Errorf("upserting traits during merge: %w", err)
		}

		// Step 3: Delete orphaned traits belonging to source segments
		// (those already merged or not conflicting are handled above).
		deleteTraitsSQL := fmt.Sprintf(
			`DELETE FROM %s WHERE graph_id = ANY($1)`,
			tableIdentityTraits,
		)
		_, err = tx.ExecContext(ctx, deleteTraitsSQL, pq.Array(sourceSegmentIDs))
		if err != nil {
			r.logger.Errorn("Error deleting source traits during merge",
				logger.NewIntField("targetSegmentID", targetSegmentID),
				obskit.Error(err),
			)
			return fmt.Errorf("deleting source traits during merge: %w", err)
		}

		// Step 4: Delete source graph segments.
		deleteSegmentsSQL := fmt.Sprintf(
			`DELETE FROM %s WHERE id = ANY($1)`,
			tableIdentityGraph,
		)
		_, err = tx.ExecContext(ctx, deleteSegmentsSQL, pq.Array(sourceSegmentIDs))
		if err != nil {
			r.logger.Errorn("Error deleting source segments during merge",
				logger.NewIntField("targetSegmentID", targetSegmentID),
				obskit.Error(err),
			)
			return fmt.Errorf("deleting source segments during merge: %w", err)
		}

		r.logger.Infon("Successfully merged segments",
			logger.NewIntField("targetSegmentID", targetSegmentID),
			logger.NewIntField("sourceCount", int64(len(sourceSegmentIDs))),
		)
		return nil
	})
}

// AddExternalID inserts a single external identifier into the identity_external_ids table
// and returns the newly created row's ID.
func (r *PostgresRepository) AddExternalID(ctx context.Context, externalID ExternalID) (int64, error) {
	now := time.Now()
	sqlStmt := fmt.Sprintf(
		`INSERT INTO %s (graph_id, external_id_type, external_id_value, created_source, created_at) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		tableIdentityExternalIDs,
	)

	var id int64
	err := r.db.QueryRowContext(ctx, sqlStmt,
		externalID.GraphID,
		externalID.ExternalIDType,
		externalID.ExternalIDValue,
		externalID.CreatedSource,
		now,
	).Scan(&id)
	if err != nil {
		r.logger.Errorn("Error adding external ID",
			logger.NewIntField("graphID", externalID.GraphID),
			logger.NewStringField("externalIDType", externalID.ExternalIDType),
			obskit.Error(err),
		)
		return 0, fmt.Errorf("adding external ID: %w", err)
	}

	r.logger.Debugn("Added external ID",
		logger.NewIntField("id", id),
		logger.NewIntField("graphID", externalID.GraphID),
		logger.NewStringField("externalIDType", externalID.ExternalIDType),
	)
	return id, nil
}

// GetExternalIDsBySegment retrieves all external IDs associated with a given graph segment ID.
// Results are ordered by created_at ascending (oldest first).
// Nullable fields (merged_at, merged_from) are handled via sql.NullTime and sql.NullInt64.
func (r *PostgresRepository) GetExternalIDsBySegment(ctx context.Context, segmentID int64) ([]ExternalID, error) {
	sqlStmt := fmt.Sprintf(
		`SELECT id, graph_id, external_id_type, external_id_value, created_source, created_at, merged_at, merged_from FROM %s WHERE graph_id = $1 ORDER BY created_at ASC`,
		tableIdentityExternalIDs,
	)

	rows, err := r.db.QueryContext(ctx, sqlStmt, segmentID)
	if err != nil {
		r.logger.Errorn("Error querying external IDs by segment",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("querying external IDs for segment %d: %w", segmentID, err)
	}
	defer func() { _ = rows.Close() }()

	var externalIDs []ExternalID
	for rows.Next() {
		var eid ExternalID
		var mergedAt sql.NullTime
		var mergedFrom sql.NullInt64

		if err := rows.Scan(
			&eid.ID, &eid.GraphID, &eid.ExternalIDType, &eid.ExternalIDValue,
			&eid.CreatedSource, &eid.CreatedAt, &mergedAt, &mergedFrom,
		); err != nil {
			r.logger.Errorn("Error scanning external ID row",
				logger.NewIntField("segmentID", segmentID),
				obskit.Error(err),
			)
			return nil, fmt.Errorf("scanning external ID row: %w", err)
		}
		if mergedAt.Valid {
			eid.MergedAt = &mergedAt.Time
		}
		if mergedFrom.Valid {
			val := mergedFrom.Int64
			eid.MergedFrom = &val
		}
		externalIDs = append(externalIDs, eid)
	}
	if err := rows.Err(); err != nil {
		r.logger.Errorn("Error iterating external ID rows",
			logger.NewIntField("segmentID", segmentID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("iterating external ID rows: %w", err)
	}
	return externalIDs, nil
}

// LookupByExternalID finds the graph segment owning a specific external ID
// within a given workspace. Returns nil, nil if no match is found.
func (r *PostgresRepository) LookupByExternalID(ctx context.Context, workspaceID, externalIDType, externalIDValue string) (*GraphSegment, error) {
	sqlStmt := fmt.Sprintf(
		`SELECT g.id, g.workspace_id, g.segment_id, g.created_at
		 FROM %s g
		 JOIN %s e ON g.id = e.graph_id
		 WHERE g.workspace_id = $1 AND e.external_id_type = $2 AND e.external_id_value = $3
		 LIMIT 1`,
		tableIdentityGraph, tableIdentityExternalIDs,
	)

	var seg GraphSegment
	err := r.db.QueryRowContext(ctx, sqlStmt, workspaceID, externalIDType, externalIDValue).Scan(
		&seg.ID, &seg.WorkspaceID, &seg.SegmentID, &seg.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		r.logger.Errorn("Error looking up segment by external ID",
			logger.NewStringField("workspaceID", workspaceID),
			logger.NewStringField("externalIDType", externalIDType),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("looking up segment by external ID: %w", err)
	}
	return &seg, nil
}

// BulkAddExternalIDs performs a bulk insert of external IDs using PostgreSQL COPY protocol.
// This follows the exact pq.CopyIn pattern from warehouse/identity/identity.go addRules function.
func (r *PostgresRepository) BulkAddExternalIDs(ctx context.Context, externalIDs []ExternalID) error {
	if len(externalIDs) == 0 {
		return nil
	}

	txn, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		r.logger.Errorn("Error beginning transaction for bulk add external IDs",
			obskit.Error(err),
		)
		return fmt.Errorf("beginning bulk add external IDs transaction: %w", err)
	}

	stmt, err := txn.Prepare(pq.CopyIn(
		tableIdentityExternalIDs,
		"graph_id", "external_id_type", "external_id_value", "created_source", "created_at",
	))
	if err != nil {
		_ = txn.Rollback()
		r.logger.Errorn("Error preparing COPY statement for bulk add external IDs",
			obskit.Error(err),
		)
		return fmt.Errorf("preparing COPY for bulk add external IDs: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now()
	for _, eid := range externalIDs {
		createdAt := eid.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		_, err = stmt.Exec(eid.GraphID, eid.ExternalIDType, eid.ExternalIDValue, eid.CreatedSource, createdAt)
		if err != nil {
			_ = txn.Rollback()
			r.logger.Errorn("Error executing COPY row for bulk add external IDs",
				logger.NewIntField("graphID", eid.GraphID),
				logger.NewStringField("externalIDType", eid.ExternalIDType),
				obskit.Error(err),
			)
			return fmt.Errorf("executing COPY row for external ID: %w", err)
		}
	}

	// Flush the COPY buffer
	_, err = stmt.Exec()
	if err != nil {
		_ = txn.Rollback()
		r.logger.Errorn("Error flushing COPY buffer for bulk add external IDs",
			obskit.Error(err),
		)
		return fmt.Errorf("flushing COPY buffer for external IDs: %w", err)
	}

	if err := txn.Commit(); err != nil {
		r.logger.Errorn("Error committing bulk add external IDs",
			obskit.Error(err),
		)
		return fmt.Errorf("committing bulk add external IDs: %w", err)
	}

	r.logger.Infon("Bulk added external IDs",
		logger.NewIntField("count", int64(len(externalIDs))),
	)
	return nil
}

// SetTrait upserts a single trait for a graph segment.
// If a trait with the same (graph_id, key) already exists, its value and updated_at
// are updated using ON CONFLICT ... DO UPDATE.
func (r *PostgresRepository) SetTrait(ctx context.Context, graphID int64, key, value string) error {
	now := time.Now()
	sqlStmt := fmt.Sprintf(
		`INSERT INTO %s (graph_id, key, value, updated_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (graph_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		tableIdentityTraits,
	)

	_, err := r.db.ExecContext(ctx, sqlStmt, graphID, key, value, now)
	if err != nil {
		r.logger.Errorn("Error setting trait",
			logger.NewIntField("graphID", graphID),
			logger.NewStringField("key", key),
			obskit.Error(err),
		)
		return fmt.Errorf("setting trait (graphID=%d, key=%s): %w", graphID, key, err)
	}
	return nil
}

// GetTraits retrieves all traits for a given graph segment ID.
// Results are ordered alphabetically by key.
func (r *PostgresRepository) GetTraits(ctx context.Context, graphID int64) ([]Trait, error) {
	sqlStmt := fmt.Sprintf(
		`SELECT id, graph_id, key, value, updated_at FROM %s WHERE graph_id = $1 ORDER BY key ASC`,
		tableIdentityTraits,
	)

	rows, err := r.db.QueryContext(ctx, sqlStmt, graphID)
	if err != nil {
		r.logger.Errorn("Error querying traits",
			logger.NewIntField("graphID", graphID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("querying traits for graphID %d: %w", graphID, err)
	}
	defer func() { _ = rows.Close() }()

	var traits []Trait
	for rows.Next() {
		var t Trait
		if err := rows.Scan(&t.ID, &t.GraphID, &t.Key, &t.Value, &t.UpdatedAt); err != nil {
			r.logger.Errorn("Error scanning trait row",
				logger.NewIntField("graphID", graphID),
				obskit.Error(err),
			)
			return nil, fmt.Errorf("scanning trait row: %w", err)
		}
		traits = append(traits, t)
	}
	if err := rows.Err(); err != nil {
		r.logger.Errorn("Error iterating trait rows",
			logger.NewIntField("graphID", graphID),
			obskit.Error(err),
		)
		return nil, fmt.Errorf("iterating trait rows: %w", err)
	}
	return traits, nil
}

// BulkSetTraits performs a bulk upsert of traits using PostgreSQL COPY protocol
// with a staging table approach. This follows the staging table pattern from
// warehouse/identity/identity.go addRules function.
//
// The approach:
// 1. Create a temp staging table (dropped on commit)
// 2. COPY traits into the staging table
// 3. Upsert from staging into the main traits table
func (r *PostgresRepository) BulkSetTraits(ctx context.Context, traits []Trait) error {
	if len(traits) == 0 {
		return nil
	}

	txn, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		r.logger.Errorn("Error beginning transaction for bulk set traits",
			obskit.Error(err),
		)
		return fmt.Errorf("beginning bulk set traits transaction: %w", err)
	}

	// Step 1: Create staging table mirroring identity_traits, dropped on commit.
	stagingTable := fmt.Sprintf("identity_traits_staging_%s", shortUUID())
	createStagingSQL := fmt.Sprintf(
		`CREATE TEMP TABLE %s (LIKE %s INCLUDING DEFAULTS) ON COMMIT DROP`,
		stagingTable, tableIdentityTraits,
	)
	_, err = txn.ExecContext(ctx, createStagingSQL)
	if err != nil {
		_ = txn.Rollback()
		r.logger.Errorn("Error creating staging table for bulk set traits",
			logger.NewStringField("stagingTable", stagingTable),
			obskit.Error(err),
		)
		return fmt.Errorf("creating staging table for bulk set traits: %w", err)
	}

	// Step 2: COPY traits into the staging table using pq.CopyIn.
	stmt, err := txn.Prepare(pq.CopyIn(
		stagingTable,
		"graph_id", "key", "value", "updated_at",
	))
	if err != nil {
		_ = txn.Rollback()
		r.logger.Errorn("Error preparing COPY statement for bulk set traits",
			obskit.Error(err),
		)
		return fmt.Errorf("preparing COPY for bulk set traits: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now()
	for _, t := range traits {
		updatedAt := t.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = now
		}
		_, err = stmt.Exec(t.GraphID, t.Key, t.Value, updatedAt)
		if err != nil {
			_ = txn.Rollback()
			r.logger.Errorn("Error executing COPY row for bulk set traits",
				logger.NewIntField("graphID", t.GraphID),
				logger.NewStringField("key", t.Key),
				obskit.Error(err),
			)
			return fmt.Errorf("executing COPY row for trait: %w", err)
		}
	}

	// Flush the COPY buffer
	_, err = stmt.Exec()
	if err != nil {
		_ = txn.Rollback()
		r.logger.Errorn("Error flushing COPY buffer for bulk set traits",
			obskit.Error(err),
		)
		return fmt.Errorf("flushing COPY buffer for traits: %w", err)
	}

	// Step 3: Upsert from staging into the main table.
	upsertSQL := fmt.Sprintf(
		`INSERT INTO %s (graph_id, key, value, updated_at)
		 SELECT graph_id, key, value, updated_at FROM %s
		 ON CONFLICT (graph_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		tableIdentityTraits, stagingTable,
	)
	_, err = txn.ExecContext(ctx, upsertSQL)
	if err != nil {
		_ = txn.Rollback()
		r.logger.Errorn("Error upserting from staging table for bulk set traits",
			logger.NewStringField("stagingTable", stagingTable),
			obskit.Error(err),
		)
		return fmt.Errorf("upserting from staging for bulk set traits: %w", err)
	}

	if err := txn.Commit(); err != nil {
		r.logger.Errorn("Error committing bulk set traits",
			obskit.Error(err),
		)
		return fmt.Errorf("committing bulk set traits: %w", err)
	}

	r.logger.Infon("Bulk set traits completed",
		logger.NewIntField("count", int64(len(traits))),
	)
	return nil
}

// GetProfileData assembles a complete profile by querying the segment, its external IDs,
// and its traits. This enables the Profiles API (E-027) to achieve sub-200ms responses
// when combined with Redis caching in identity/profiles/cache.go.
func (r *PostgresRepository) GetProfileData(ctx context.Context, segmentID int64) (*ProfileData, error) {
	// Query the graph segment
	seg, err := r.GetSegment(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("getting profile data segment: %w", err)
	}
	if seg == nil {
		return nil, nil
	}

	// Query external IDs for this segment
	externalIDs, err := r.GetExternalIDsBySegment(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("getting profile data external IDs: %w", err)
	}
	if externalIDs == nil {
		externalIDs = []ExternalID{}
	}

	// Query traits for this segment
	traits, err := r.GetTraits(ctx, segmentID)
	if err != nil {
		return nil, fmt.Errorf("getting profile data traits: %w", err)
	}
	if traits == nil {
		traits = []Trait{}
	}

	return &ProfileData{
		Segment:     *seg,
		ExternalIDs: externalIDs,
		Traits:      traits,
	}, nil
}

// WithTx executes the provided function within a database transaction.
// If fn returns an error, the transaction is rolled back. Otherwise, it is committed.
// This follows the WithTx pattern from warehouse/integrations/middleware/sqlquerywrapper/sql.go.
func (r *PostgresRepository) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	txn, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		r.logger.Errorn("Error beginning transaction",
			obskit.Error(err),
		)
		return fmt.Errorf("beginning transaction: %w", err)
	}

	if err = fn(txn); err != nil {
		if rollbackErr := txn.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			r.logger.Errorn("Error rolling back transaction",
				obskit.Error(fmt.Errorf("executing: %s, rollback: %s", err.Error(), rollbackErr.Error())),
			)
		}
		return fmt.Errorf("executing transaction: %w", err)
	}

	if err := txn.Commit(); err != nil {
		r.logger.Errorn("Error committing transaction",
			obskit.Error(err),
		)
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// Ping verifies the database connection is alive.
func (r *PostgresRepository) Ping(ctx context.Context) error {
	if err := r.db.PingContext(ctx); err != nil {
		r.logger.Errorn("Database ping failed",
			obskit.Error(err),
		)
		return fmt.Errorf("pinging database: %w", err)
	}
	return nil
}

// shortUUID returns the first 8 characters of a new UUID for use in temp table names.
func shortUUID() string {
	id := uuid.New().String()
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
