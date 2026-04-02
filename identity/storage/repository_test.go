// Package storage tests validate PostgreSQL persistence for the identity graph.
//
// This file provides comprehensive unit tests for the PostgresRepository using
// go-sqlmock, covering segment CRUD operations, external ID management, trait
// upserts, composite profile assembly, transaction handling, health checks,
// workspace isolation, nullable field handling (merged_at, merged_from), and
// error propagation. Tests follow the protocols/storage/repository_test.go
// sqlmock pattern.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/logger"
)

// --------------------------------------------------------------------------
// Test helpers
// --------------------------------------------------------------------------

// newTestRepo creates a PostgresRepository backed by go-sqlmock with regexp matching.
// Uses logger.NOP to suppress log output, matching the protocols/storage test pattern.
func newTestRepo(t *testing.T) (*PostgresRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresRepository(db, logger.NOP), mock
}

// segCols returns column names for identity_graph SELECT queries.
func segCols() []string {
	return []string{"id", "workspace_id", "segment_id", "created_at"}
}

// extIDCols returns column names for identity_external_ids SELECT queries.
func extIDCols() []string {
	return []string{"id", "graph_id", "workspace_id", "external_id_type", "external_id_value", "created_source", "created_at", "merged_at", "merged_from"}
}

// traitCols returns column names for identity_traits SELECT queries.
func traitCols() []string {
	return []string{"id", "graph_id", "key", "value", "updated_at"}
}

// fixedTime returns a deterministic timestamp for reproducible assertions.
func fixedTime() time.Time {
	return time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC)
}

// --------------------------------------------------------------------------
// Constructor Tests
// --------------------------------------------------------------------------

func TestNewPostgresRepository(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := NewPostgresRepository(db, logger.NOP)
	require.NotNil(t, repo)
	require.NotNil(t, repo.db)
	require.NotNil(t, repo.logger)
}

func TestNewPostgresRepository_NilLogger(t *testing.T) {
	// When log is nil, the package-level logger should be used.
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := NewPostgresRepository(db, nil)
	require.NotNil(t, repo)
	require.NotNil(t, repo.logger)
}

// --------------------------------------------------------------------------
// CreateSegment Tests
// --------------------------------------------------------------------------

func TestCreateSegment_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO " + tableIdentityGraph)).
		WithArgs("ws-001", sqlmock.AnyArg(), sqlmock.AnyArg()). // workspace_id, segment_id (uuid), created_at
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(100)))

	id, err := repo.CreateSegment(ctx, "ws-001")
	require.NoError(t, err)
	require.Equal(t, int64(100), id)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateSegment_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO " + tableIdentityGraph)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(errors.New("disk full"))

	id, err := repo.CreateSegment(ctx, "ws-001")
	require.Error(t, err)
	require.Equal(t, int64(0), id)
	require.Contains(t, err.Error(), "creating segment")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// GetSegment Tests
// --------------------------------------------------------------------------

func TestGetSegment_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workspace_id, segment_id, created_at FROM " + tableIdentityGraph)).
		WithArgs(int64(100)).
		WillReturnRows(
			sqlmock.NewRows(segCols()).
				AddRow(int64(100), "ws-001", "seg-uuid-001", now),
		)

	seg, err := repo.GetSegment(ctx, 100)
	require.NoError(t, err)
	require.NotNil(t, seg)
	require.Equal(t, int64(100), seg.ID)
	require.Equal(t, "ws-001", seg.WorkspaceID)
	require.Equal(t, "seg-uuid-001", seg.SegmentID)
	require.Equal(t, now, seg.CreatedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetSegment_NotFound(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workspace_id, segment_id, created_at FROM " + tableIdentityGraph)).
		WithArgs(int64(999)).
		WillReturnError(sql.ErrNoRows)

	seg, err := repo.GetSegment(ctx, 999)
	require.NoError(t, err, "GetSegment returns nil, nil for not found")
	require.Nil(t, seg)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetSegment_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workspace_id, segment_id, created_at FROM " + tableIdentityGraph)).
		WithArgs(int64(100)).
		WillReturnError(errors.New("connection refused"))

	seg, err := repo.GetSegment(ctx, 100)
	require.Error(t, err)
	require.Nil(t, seg)
	require.Contains(t, err.Error(), "getting segment 100")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// GetSegmentByWorkspace Tests
// --------------------------------------------------------------------------

func TestGetSegmentByWorkspace_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT id, workspace_id, segment_id, created_at FROM "+tableIdentityGraph+" WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3",
	)).
		WithArgs("ws-001", 10, 0).
		WillReturnRows(
			sqlmock.NewRows(segCols()).
				AddRow(int64(102), "ws-001", "seg-002", now).
				AddRow(int64(101), "ws-001", "seg-001", now.Add(-time.Hour)),
		)

	segs, err := repo.GetSegmentByWorkspace(ctx, "ws-001", 10, 0)
	require.NoError(t, err)
	require.Len(t, segs, 2)
	require.Equal(t, int64(102), segs[0].ID)
	require.Equal(t, int64(101), segs[1].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetSegmentByWorkspace_Empty(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT id, workspace_id, segment_id, created_at FROM "+tableIdentityGraph+" WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3",
	)).
		WithArgs("ws-empty", 50, 0).
		WillReturnRows(sqlmock.NewRows(segCols()))

	segs, err := repo.GetSegmentByWorkspace(ctx, "ws-empty", 50, 0)
	require.NoError(t, err)
	require.Nil(t, segs) // nil because append was never called on nil slice
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetSegmentByWorkspace_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT id, workspace_id, segment_id, created_at FROM " + tableIdentityGraph,
	)).
		WithArgs("ws-001", 10, 0).
		WillReturnError(errors.New("connection lost"))

	segs, err := repo.GetSegmentByWorkspace(ctx, "ws-001", 10, 0)
	require.Error(t, err)
	require.Nil(t, segs)
	require.Contains(t, err.Error(), "querying segments by workspace")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// AddExternalID Tests
// --------------------------------------------------------------------------

func TestAddExternalID_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	eid := ExternalID{
		GraphID:         100,
		WorkspaceID:     "ws-001",
		ExternalIDType:  "email",
		ExternalIDValue: "user@example.com",
		CreatedSource:   "identify",
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO " + tableIdentityExternalIDs)).
		WithArgs(eid.GraphID, eid.WorkspaceID, eid.ExternalIDType, eid.ExternalIDValue, eid.CreatedSource, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(200)))

	id, err := repo.AddExternalID(ctx, eid)
	require.NoError(t, err)
	require.Equal(t, int64(200), id)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAddExternalID_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	eid := ExternalID{
		GraphID:         100,
		WorkspaceID:     "ws-001",
		ExternalIDType:  "email",
		ExternalIDValue: "dup@example.com",
		CreatedSource:   "identify",
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO " + tableIdentityExternalIDs)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(errors.New("unique constraint violation"))

	id, err := repo.AddExternalID(ctx, eid)
	require.Error(t, err)
	require.Equal(t, int64(0), id)
	require.Contains(t, err.Error(), "adding external ID")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// GetExternalIDsBySegment Tests
// --------------------------------------------------------------------------

func TestGetExternalIDsBySegment_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mergedAt := now.Add(time.Hour)
	mergedFrom := int64(50)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id, workspace_id, external_id_type, external_id_value, created_source, created_at, merged_at, merged_from FROM " + tableIdentityExternalIDs)).
		WithArgs(int64(100)).
		WillReturnRows(
			sqlmock.NewRows(extIDCols()).
				AddRow(int64(200), int64(100), "ws-001", "email", "user@example.com", "identify", now, nil, nil).
				AddRow(int64(201), int64(100), "ws-001", "user_id", "uid-001", "track", now, mergedAt, mergedFrom),
		)

	eids, err := repo.GetExternalIDsBySegment(ctx, 100)
	require.NoError(t, err)
	require.Len(t, eids, 2)

	// First external ID: no merge data
	require.Equal(t, int64(200), eids[0].ID)
	require.Equal(t, "email", eids[0].ExternalIDType)
	require.Equal(t, "user@example.com", eids[0].ExternalIDValue)
	require.Nil(t, eids[0].MergedAt)
	require.Nil(t, eids[0].MergedFrom)

	// Second external ID: with merge data (nullable fields populated)
	require.Equal(t, int64(201), eids[1].ID)
	require.Equal(t, "user_id", eids[1].ExternalIDType)
	require.NotNil(t, eids[1].MergedAt)
	require.Equal(t, mergedAt, *eids[1].MergedAt)
	require.NotNil(t, eids[1].MergedFrom)
	require.Equal(t, mergedFrom, *eids[1].MergedFrom)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetExternalIDsBySegment_Empty(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id, workspace_id, external_id_type, external_id_value, created_source, created_at, merged_at, merged_from FROM " + tableIdentityExternalIDs)).
		WithArgs(int64(999)).
		WillReturnRows(sqlmock.NewRows(extIDCols()))

	eids, err := repo.GetExternalIDsBySegment(ctx, 999)
	require.NoError(t, err)
	require.Nil(t, eids)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetExternalIDsBySegment_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id")).
		WithArgs(int64(100)).
		WillReturnError(errors.New("table not found"))

	eids, err := repo.GetExternalIDsBySegment(ctx, 100)
	require.Error(t, err)
	require.Nil(t, eids)
	require.Contains(t, err.Error(), "querying external IDs for segment")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// LookupByExternalID Tests
// --------------------------------------------------------------------------

func TestLookupByExternalID_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT g.id, g.workspace_id, g.segment_id, g.created_at")).
		WithArgs("ws-001", "email", "user@example.com").
		WillReturnRows(
			sqlmock.NewRows(segCols()).
				AddRow(int64(100), "ws-001", "seg-uuid-001", now),
		)

	seg, err := repo.LookupByExternalID(ctx, "ws-001", "email", "user@example.com")
	require.NoError(t, err)
	require.NotNil(t, seg)
	require.Equal(t, int64(100), seg.ID)
	require.Equal(t, "ws-001", seg.WorkspaceID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLookupByExternalID_NotFound(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT g.id, g.workspace_id, g.segment_id, g.created_at")).
		WithArgs("ws-001", "email", "missing@example.com").
		WillReturnError(sql.ErrNoRows)

	seg, err := repo.LookupByExternalID(ctx, "ws-001", "email", "missing@example.com")
	require.NoError(t, err, "LookupByExternalID returns nil, nil for not found")
	require.Nil(t, seg)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLookupByExternalID_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT g.id")).
		WithArgs("ws-001", "email", "user@example.com").
		WillReturnError(errors.New("connection timeout"))

	seg, err := repo.LookupByExternalID(ctx, "ws-001", "email", "user@example.com")
	require.Error(t, err)
	require.Nil(t, seg)
	require.Contains(t, err.Error(), "looking up segment by external ID")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// SetTrait Tests
// --------------------------------------------------------------------------

func TestSetTrait_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO " + tableIdentityTraits)).
		WithArgs(int64(100), "email", "user@example.com", sqlmock.AnyArg()). // graph_id, key, value, updated_at
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repo.SetTrait(ctx, 100, "email", "user@example.com")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSetTrait_UpsertExisting(t *testing.T) {
	// SetTrait uses ON CONFLICT ... DO UPDATE — verify it still succeeds when
	// the key already exists for the graph segment (upsert path).
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO " + tableIdentityTraits)).
		WithArgs(int64(100), "name", "Updated Name", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1)) // 1 row affected (updated)

	err := repo.SetTrait(ctx, 100, "name", "Updated Name")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSetTrait_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO " + tableIdentityTraits)).
		WithArgs(int64(100), "key", "val", sqlmock.AnyArg()).
		WillReturnError(errors.New("constraint error"))

	err := repo.SetTrait(ctx, 100, "key", "val")
	require.Error(t, err)
	require.Contains(t, err.Error(), "setting trait")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// GetTraits Tests
// --------------------------------------------------------------------------

func TestGetTraits_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id, key, value, updated_at FROM " + tableIdentityTraits)).
		WithArgs(int64(100)).
		WillReturnRows(
			sqlmock.NewRows(traitCols()).
				AddRow(int64(300), int64(100), "email", "user@example.com", now).
				AddRow(int64(301), int64(100), "name", "Test User", now),
		)

	traits, err := repo.GetTraits(ctx, 100)
	require.NoError(t, err)
	require.Len(t, traits, 2)
	require.Equal(t, "email", traits[0].Key)
	require.Equal(t, "user@example.com", traits[0].Value)
	require.Equal(t, "name", traits[1].Key)
	require.Equal(t, "Test User", traits[1].Value)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetTraits_Empty(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id, key, value, updated_at FROM " + tableIdentityTraits)).
		WithArgs(int64(999)).
		WillReturnRows(sqlmock.NewRows(traitCols()))

	traits, err := repo.GetTraits(ctx, 999)
	require.NoError(t, err)
	require.Nil(t, traits)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetTraits_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id, key, value, updated_at FROM " + tableIdentityTraits)).
		WithArgs(int64(100)).
		WillReturnError(errors.New("disk error"))

	traits, err := repo.GetTraits(ctx, 100)
	require.Error(t, err)
	require.Nil(t, traits)
	require.Contains(t, err.Error(), "querying traits")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// GetProfileData Tests
// --------------------------------------------------------------------------

func TestGetProfileData_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()

	// Mock GetSegment
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workspace_id, segment_id, created_at FROM " + tableIdentityGraph)).
		WithArgs(int64(100)).
		WillReturnRows(
			sqlmock.NewRows(segCols()).
				AddRow(int64(100), "ws-001", "seg-uuid-001", now),
		)

	// Mock GetExternalIDsBySegment
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id, workspace_id, external_id_type, external_id_value, created_source, created_at, merged_at, merged_from FROM " + tableIdentityExternalIDs)).
		WithArgs(int64(100)).
		WillReturnRows(
			sqlmock.NewRows(extIDCols()).
				AddRow(int64(200), int64(100), "ws-001", "email", "user@example.com", "identify", now, nil, nil),
		)

	// Mock GetTraits
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id, key, value, updated_at FROM " + tableIdentityTraits)).
		WithArgs(int64(100)).
		WillReturnRows(
			sqlmock.NewRows(traitCols()).
				AddRow(int64(300), int64(100), "name", "Test User", now),
		)

	profile, err := repo.GetProfileData(ctx, 100)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.Equal(t, int64(100), profile.Segment.ID)
	require.Equal(t, "ws-001", profile.Segment.WorkspaceID)
	require.Len(t, profile.ExternalIDs, 1)
	require.Equal(t, "email", profile.ExternalIDs[0].ExternalIDType)
	require.Len(t, profile.Traits, 1)
	require.Equal(t, "name", profile.Traits[0].Key)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetProfileData_SegmentNotFound(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	// GetSegment returns nil, nil for not found
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workspace_id, segment_id, created_at FROM " + tableIdentityGraph)).
		WithArgs(int64(999)).
		WillReturnError(sql.ErrNoRows)

	profile, err := repo.GetProfileData(ctx, 999)
	require.NoError(t, err)
	require.Nil(t, profile)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetProfileData_EmptyExternalIDsAndTraits(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()

	// Mock GetSegment — segment exists
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workspace_id, segment_id, created_at FROM " + tableIdentityGraph)).
		WithArgs(int64(100)).
		WillReturnRows(
			sqlmock.NewRows(segCols()).
				AddRow(int64(100), "ws-001", "seg-uuid-001", now),
		)

	// Mock GetExternalIDsBySegment — empty result
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id, workspace_id, external_id_type, external_id_value, created_source, created_at, merged_at, merged_from FROM " + tableIdentityExternalIDs)).
		WithArgs(int64(100)).
		WillReturnRows(sqlmock.NewRows(extIDCols()))

	// Mock GetTraits — empty result
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id, key, value, updated_at FROM " + tableIdentityTraits)).
		WithArgs(int64(100)).
		WillReturnRows(sqlmock.NewRows(traitCols()))

	profile, err := repo.GetProfileData(ctx, 100)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.Equal(t, int64(100), profile.Segment.ID)
	// GetProfileData normalizes nil slices to empty slices
	require.NotNil(t, profile.ExternalIDs)
	require.Len(t, profile.ExternalIDs, 0)
	require.NotNil(t, profile.Traits)
	require.Len(t, profile.Traits, 0)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetProfileData_ExternalIDsError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()

	// Mock GetSegment — success
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, workspace_id, segment_id, created_at FROM " + tableIdentityGraph)).
		WithArgs(int64(100)).
		WillReturnRows(
			sqlmock.NewRows(segCols()).
				AddRow(int64(100), "ws-001", "seg-uuid-001", now),
		)

	// Mock GetExternalIDsBySegment — error
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, graph_id")).
		WithArgs(int64(100)).
		WillReturnError(errors.New("external IDs query failed"))

	profile, err := repo.GetProfileData(ctx, 100)
	require.Error(t, err)
	require.Nil(t, profile)
	require.Contains(t, err.Error(), "getting profile data external IDs")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Ping Tests
// --------------------------------------------------------------------------

// newTestRepoWithPing creates a repo with MonitorPingsOption enabled so
// ExpectPing works correctly with go-sqlmock v1.5.2.
func newTestRepoWithPing(t *testing.T) (*PostgresRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(true),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgresRepository(db, logger.NOP), mock
}

func TestPing_Success(t *testing.T) {
	repo, mock := newTestRepoWithPing(t)
	ctx := context.Background()

	mock.ExpectPing()

	err := repo.Ping(ctx)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPing_Error(t *testing.T) {
	repo, mock := newTestRepoWithPing(t)
	ctx := context.Background()

	mock.ExpectPing().WillReturnError(errors.New("database unreachable"))

	err := repo.Ping(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pinging database")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// WithTx Tests
// --------------------------------------------------------------------------

func TestWithTx_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectExec("SELECT 1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	err := repo.WithTx(ctx, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, "SELECT 1")
		return execErr
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithTx_FnError_RollsBack(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectRollback()

	err := repo.WithTx(ctx, func(_ *sql.Tx) error {
		return errors.New("business logic error")
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "executing transaction")
	require.Contains(t, err.Error(), "business logic error")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithTx_BeginError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectBegin().WillReturnError(errors.New("too many connections"))

	err := repo.WithTx(ctx, func(_ *sql.Tx) error {
		return nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "beginning transaction")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithTx_CommitError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))

	err := repo.WithTx(ctx, func(_ *sql.Tx) error {
		return nil // fn succeeds but commit fails
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "committing transaction")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Bulk Operation Empty Input Tests
// --------------------------------------------------------------------------

func TestBulkAddExternalIDs_EmptyInput(t *testing.T) {
	// BulkAddExternalIDs with empty slice should be a no-op.
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	err := repo.BulkAddExternalIDs(ctx, []ExternalID{})
	require.NoError(t, err)
}

func TestBulkSetTraits_EmptyInput(t *testing.T) {
	// BulkSetTraits with empty slice should be a no-op.
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	err := repo.BulkSetTraits(ctx, []Trait{})
	require.NoError(t, err)
}

func TestMergeSegments_EmptySourceIDs(t *testing.T) {
	// MergeSegments with empty sourceSegmentIDs should be a no-op.
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	err := repo.MergeSegments(ctx, 100, []int64{})
	require.NoError(t, err)
}

// --------------------------------------------------------------------------
// shortUUID Tests
// --------------------------------------------------------------------------

func TestShortUUID_ReturnsEightChars(t *testing.T) {
	id := shortUUID()
	require.Len(t, id, 8, "shortUUID should return 8 characters")
}

func TestShortUUID_Unique(t *testing.T) {
	// Generate multiple short UUIDs and verify they are unique.
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := shortUUID()
		require.False(t, seen[id], "shortUUID should generate unique values, got duplicate: %s", id)
		seen[id] = true
	}
}
