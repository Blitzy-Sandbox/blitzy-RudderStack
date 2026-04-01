// Package storage tests validate PostgreSQL persistence for tracking plans.
//
// This file provides comprehensive unit tests for the Repository using go-sqlmock,
// covering CRUD operations for tracking plans and version history, workspace
// isolation, JSON validation, error handling, and concurrent access patterns.
// Tests follow the warehouse/healthmonitor/monitor_test.go sqlmock pattern.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// --------------------------------------------------------------------------
// Test helpers
// --------------------------------------------------------------------------

// newTestRepo creates a Repository backed by go-sqlmock with regexp matching.
// The caller must defer db.Close() or use t.Cleanup.
func newTestRepo(t *testing.T) (*Repository, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewRepository(db), mock, db
}

// sampleSchema returns a minimal valid JSON Schema draft-07 for testing.
func sampleSchema() []byte {
	return []byte(`{"type":"object","properties":{"event":{"type":"string"}},"required":["event"]}`)
}

// sampleEnforcementConfig returns a valid enforcement config for testing.
func sampleEnforcementConfig() []byte {
	return []byte(`{"track":"block","identify":"allow","page":"omit"}`)
}

// trackingPlanColumns returns the column names used by SELECT queries.
func trackingPlanCols() []string {
	return []string{"id", "workspace_id", "name", "schema", "version", "enforcement_config", "created_at", "updated_at"}
}

// trackingPlanVersionCols returns the column names for version SELECT queries.
func trackingPlanVersionCols() []string {
	return []string{"id", "tracking_plan_id", "version", "schema", "changelog", "created_at"}
}

// --------------------------------------------------------------------------
// Phase 1: Constructor
// --------------------------------------------------------------------------

func TestNewRepository(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := NewRepository(db)
	require.NotNil(t, repo)
	require.Equal(t, db, repo.db)
}

// --------------------------------------------------------------------------
// Phase 2: Create tracking plan
// --------------------------------------------------------------------------

func TestCreate_Success(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		WorkspaceID:       "ws-001",
		Name:              "User Events Plan",
		Schema:            sampleSchema(),
		Version:           1,
		EnforcementConfig: sampleEnforcementConfig(),
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+trackingPlansTable)).
		WithArgs(tp.WorkspaceID, tp.Name, sqlmock.AnyArg(), tp.Version, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tp-100"))

	id, err := repo.Create(ctx, tp)
	require.NoError(t, err)
	require.Equal(t, "tp-100", id)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreate_InvalidSchemaJSON(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		WorkspaceID: "ws-001",
		Name:        "Bad Schema",
		Schema:      []byte(`{not valid json`),
		Version:     1,
	}

	_, err := repo.Create(ctx, tp)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid JSON in schema")
}

func TestCreate_InvalidEnforcementJSON(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		WorkspaceID:       "ws-001",
		Name:              "Bad Enforcement",
		Schema:            sampleSchema(),
		Version:           1,
		EnforcementConfig: []byte(`not json`),
	}

	_, err := repo.Create(ctx, tp)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid JSON in enforcement_config")
}

func TestCreate_EmptyOptionalFields(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		WorkspaceID: "ws-001",
		Name:        "Minimal Plan",
		Version:     1,
		// Schema and EnforcementConfig intentionally nil — treated as optional
	}

	// marshalJSONField(nil) returns nil []byte, but the Go database/sql driver
	// normalises nil []byte to empty []byte{} — use AnyArg for schema/enforcement.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+trackingPlansTable)).
		WithArgs(tp.WorkspaceID, tp.Name, sqlmock.AnyArg(), tp.Version, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tp-101"))

	id, err := repo.Create(ctx, tp)
	require.NoError(t, err)
	require.Equal(t, "tp-101", id)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreate_DBError(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		WorkspaceID: "ws-001",
		Name:        "Plan",
		Version:     1,
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+trackingPlansTable)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(fmt.Errorf("connection refused"))

	_, err := repo.Create(ctx, tp)
	require.Error(t, err)
	require.Contains(t, err.Error(), "creating tracking plan")
}

// --------------------------------------------------------------------------
// Phase 3: Get tracking plan
// --------------------------------------------------------------------------

func TestGet_Found(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns+" FROM "+trackingPlansTable)).
		WithArgs("tp-100").
		WillReturnRows(
			sqlmock.NewRows(trackingPlanCols()).
				AddRow("tp-100", "ws-001", "User Events", string(sampleSchema()), 2, string(sampleEnforcementConfig()), now, now),
		)

	tp, err := repo.Get(ctx, "tp-100")
	require.NoError(t, err)
	require.Equal(t, "tp-100", tp.ID)
	require.Equal(t, "ws-001", tp.WorkspaceID)
	require.Equal(t, "User Events", tp.Name)
	require.Equal(t, 2, tp.Version)
	require.JSONEq(t, string(sampleSchema()), string(tp.Schema))
	require.JSONEq(t, string(sampleEnforcementConfig()), string(tp.EnforcementConfig))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGet_NotFound(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns+" FROM "+trackingPlansTable)).
		WithArgs("nonexistent").
		WillReturnError(sql.ErrNoRows)

	_, err := repo.Get(ctx, "nonexistent")
	require.ErrorIs(t, err, ErrTrackingPlanNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGet_NullableFields(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	// Schema and enforcement_config stored as NULL in DB
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns+" FROM "+trackingPlansTable)).
		WithArgs("tp-200").
		WillReturnRows(
			sqlmock.NewRows(trackingPlanCols()).
				AddRow("tp-200", "ws-001", "Empty Schema", nil, 1, nil, now, now),
		)

	tp, err := repo.Get(ctx, "tp-200")
	require.NoError(t, err)
	require.Equal(t, "tp-200", tp.ID)
	require.Nil(t, tp.Schema)
	require.Nil(t, tp.EnforcementConfig)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGet_DBError(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns)).
		WithArgs("tp-err").
		WillReturnError(fmt.Errorf("db timeout"))

	_, err := repo.Get(ctx, "tp-err")
	require.Error(t, err)
	require.Contains(t, err.Error(), "getting tracking plan")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Phase 4: GetByWorkspace
// --------------------------------------------------------------------------

func TestGetByWorkspace_Found(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns+" FROM "+trackingPlansTable+" WHERE workspace_id")).
		WithArgs("ws-001", "tp-300").
		WillReturnRows(
			sqlmock.NewRows(trackingPlanCols()).
				AddRow("tp-300", "ws-001", "Plan X", string(sampleSchema()), 1, nil, now, now),
		)

	tp, err := repo.GetByWorkspace(ctx, "ws-001", "tp-300")
	require.NoError(t, err)
	require.Equal(t, "tp-300", tp.ID)
	require.Equal(t, "ws-001", tp.WorkspaceID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetByWorkspace_NotFound(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns+" FROM "+trackingPlansTable+" WHERE workspace_id")).
		WithArgs("ws-002", "tp-300").
		WillReturnError(sql.ErrNoRows)

	_, err := repo.GetByWorkspace(ctx, "ws-002", "tp-300")
	require.ErrorIs(t, err, ErrTrackingPlanNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetByWorkspace_WorkspaceIsolation(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	// Plan exists in ws-001 but request is for ws-002 — should return not found.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns+" FROM "+trackingPlansTable+" WHERE workspace_id")).
		WithArgs("ws-002", "tp-300").
		WillReturnError(sql.ErrNoRows)

	_, err := repo.GetByWorkspace(ctx, "ws-002", "tp-300")
	require.ErrorIs(t, err, ErrTrackingPlanNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Phase 5: Update tracking plan
// --------------------------------------------------------------------------

func TestUpdate_Success(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		ID:                "tp-100",
		WorkspaceID:       "ws-001",
		Name:              "Updated Plan",
		Schema:            sampleSchema(),
		Version:           2,
		EnforcementConfig: sampleEnforcementConfig(),
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE "+trackingPlansTable)).
		WithArgs(tp.Name, sqlmock.AnyArg(), tp.Version, sqlmock.AnyArg(), sqlmock.AnyArg(), tp.ID, tp.WorkspaceID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.Update(ctx, tp)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdate_NotFound(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		ID:          "tp-nonexistent",
		WorkspaceID: "ws-001",
		Name:        "No Match",
		Version:     1,
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE "+trackingPlansTable)).
		WithArgs(tp.Name, sqlmock.AnyArg(), tp.Version, sqlmock.AnyArg(), sqlmock.AnyArg(), tp.ID, tp.WorkspaceID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.Update(ctx, tp)
	require.ErrorIs(t, err, ErrTrackingPlanNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdate_InvalidSchemaJSON(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		ID:          "tp-100",
		WorkspaceID: "ws-001",
		Name:        "Bad Schema",
		Schema:      []byte(`{broken`),
		Version:     2,
	}

	err := repo.Update(ctx, tp)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid JSON in schema")
}

func TestUpdate_InvalidEnforcementJSON(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		ID:                "tp-100",
		WorkspaceID:       "ws-001",
		Name:              "Bad Enforcement",
		Schema:            sampleSchema(),
		Version:           2,
		EnforcementConfig: []byte(`not-json`),
	}

	err := repo.Update(ctx, tp)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid JSON in enforcement_config")
}

func TestUpdate_DBError(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	tp := TrackingPlan{
		ID:          "tp-100",
		WorkspaceID: "ws-001",
		Name:        "Plan",
		Version:     2,
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE "+trackingPlansTable)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(fmt.Errorf("deadlock detected"))

	err := repo.Update(ctx, tp)
	require.Error(t, err)
	require.Contains(t, err.Error(), "updating tracking plan")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Phase 6: Delete tracking plan
// --------------------------------------------------------------------------

func TestDelete_Success(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM "+trackingPlansTable)).
		WithArgs("tp-100", "ws-001").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.Delete(ctx, "ws-001", "tp-100")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDelete_NotFound(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM "+trackingPlansTable)).
		WithArgs("tp-nonexistent", "ws-001").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.Delete(ctx, "ws-001", "tp-nonexistent")
	require.ErrorIs(t, err, ErrTrackingPlanNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDelete_DBError(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM "+trackingPlansTable)).
		WithArgs("tp-err", "ws-001").
		WillReturnError(fmt.Errorf("connection reset"))

	err := repo.Delete(ctx, "ws-001", "tp-err")
	require.Error(t, err)
	require.Contains(t, err.Error(), "deleting tracking plan")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Phase 7: List tracking plans
// --------------------------------------------------------------------------

func TestList_MultipleResults(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	rows := sqlmock.NewRows(trackingPlanCols()).
		AddRow("tp-1", "ws-001", "Plan A", string(sampleSchema()), 1, nil, now, now).
		AddRow("tp-2", "ws-001", "Plan B", nil, 2, string(sampleEnforcementConfig()), now.Add(-time.Hour), now)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns+" FROM "+trackingPlansTable+" WHERE workspace_id")).
		WithArgs("ws-001", 100, 0).
		WillReturnRows(rows)

	plans, err := repo.List(ctx, "ws-001", 100, 0)
	require.NoError(t, err)
	require.Len(t, plans, 2)
	require.Equal(t, "Plan A", plans[0].Name)
	require.JSONEq(t, string(sampleSchema()), string(plans[0].Schema))
	require.Equal(t, "Plan B", plans[1].Name)
	require.Nil(t, plans[1].Schema)
	require.JSONEq(t, string(sampleEnforcementConfig()), string(plans[1].EnforcementConfig))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_EmptyResult(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns+" FROM "+trackingPlansTable+" WHERE workspace_id")).
		WithArgs("ws-empty", 100, 0).
		WillReturnRows(sqlmock.NewRows(trackingPlanCols()))

	plans, err := repo.List(ctx, "ws-empty", 100, 0)
	require.NoError(t, err)
	require.NotNil(t, plans, "List should return non-nil empty slice, not nil")
	require.Empty(t, plans)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_DBError(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns)).
		WithArgs("ws-err", 100, 0).
		WillReturnError(fmt.Errorf("table does not exist"))

	_, err := repo.List(ctx, "ws-err", 100, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "listing tracking plans")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Phase 8: CreateVersion
// --------------------------------------------------------------------------

func TestCreateVersion_Success(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	v := TrackingPlanVersion{
		TrackingPlanID: "tp-100",
		Version:        3,
		Schema:         sampleSchema(),
		Changelog:      "Added page event validation",
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+trackingPlanVersionsTable)).
		WithArgs(v.TrackingPlanID, v.Version, sqlmock.AnyArg(), v.Changelog, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ver-300"))

	id, err := repo.CreateVersion(ctx, v)
	require.NoError(t, err)
	require.Equal(t, "ver-300", id)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateVersion_InvalidSchemaJSON(t *testing.T) {
	repo, _, _ := newTestRepo(t)
	ctx := context.Background()

	v := TrackingPlanVersion{
		TrackingPlanID: "tp-100",
		Version:        1,
		Schema:         []byte(`broken json`),
	}

	_, err := repo.CreateVersion(ctx, v)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid JSON in schema")
}

func TestCreateVersion_EmptySchema(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	v := TrackingPlanVersion{
		TrackingPlanID: "tp-100",
		Version:        1,
		Changelog:      "Initial version with no schema",
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+trackingPlanVersionsTable)).
		WithArgs(v.TrackingPlanID, v.Version, sqlmock.AnyArg(), v.Changelog, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ver-301"))

	id, err := repo.CreateVersion(ctx, v)
	require.NoError(t, err)
	require.Equal(t, "ver-301", id)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateVersion_DBError(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	v := TrackingPlanVersion{
		TrackingPlanID: "tp-100",
		Version:        1,
		Schema:         sampleSchema(),
		Changelog:      "test",
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+trackingPlanVersionsTable)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(fmt.Errorf("FK violation"))

	_, err := repo.CreateVersion(ctx, v)
	require.Error(t, err)
	require.Contains(t, err.Error(), "creating tracking plan version")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Phase 9: GetVersions
// --------------------------------------------------------------------------

func TestGetVersions_MultipleResults(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	rows := sqlmock.NewRows(trackingPlanVersionCols()).
		AddRow("ver-3", "tp-100", 3, string(sampleSchema()), "v3 changes", now).
		AddRow("ver-2", "tp-100", 2, nil, "v2 changes", now.Add(-time.Hour)).
		AddRow("ver-1", "tp-100", 1, nil, "initial version", now.Add(-2*time.Hour))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanVersionColumns+" FROM "+trackingPlanVersionsTable)).
		WithArgs("tp-100").
		WillReturnRows(rows)

	versions, err := repo.GetVersions(ctx, "tp-100")
	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, 3, versions[0].Version)
	require.Equal(t, 2, versions[1].Version)
	require.Equal(t, 1, versions[2].Version)
	require.JSONEq(t, string(sampleSchema()), string(versions[0].Schema))
	require.Nil(t, versions[1].Schema)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetVersions_Empty(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanVersionColumns+" FROM "+trackingPlanVersionsTable)).
		WithArgs("tp-no-versions").
		WillReturnRows(sqlmock.NewRows(trackingPlanVersionCols()))

	versions, err := repo.GetVersions(ctx, "tp-no-versions")
	require.NoError(t, err)
	require.NotNil(t, versions, "GetVersions should return non-nil empty slice")
	require.Empty(t, versions)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetVersions_DBError(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanVersionColumns)).
		WithArgs("tp-err").
		WillReturnError(fmt.Errorf("relation does not exist"))

	_, err := repo.GetVersions(ctx, "tp-err")
	require.Error(t, err)
	require.Contains(t, err.Error(), "listing versions")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Phase 10: GetVersion
// --------------------------------------------------------------------------

func TestGetVersion_Found(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanVersionColumns+" FROM "+trackingPlanVersionsTable+" WHERE tracking_plan_id")).
		WithArgs("tp-100", 2).
		WillReturnRows(
			sqlmock.NewRows(trackingPlanVersionCols()).
				AddRow("ver-2", "tp-100", 2, string(sampleSchema()), "v2 changelog", now),
		)

	v, err := repo.GetVersion(ctx, "tp-100", 2)
	require.NoError(t, err)
	require.Equal(t, "ver-2", v.ID)
	require.Equal(t, "tp-100", v.TrackingPlanID)
	require.Equal(t, 2, v.Version)
	require.JSONEq(t, string(sampleSchema()), string(v.Schema))
	require.Equal(t, "v2 changelog", v.Changelog)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetVersion_NotFound(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanVersionColumns+" FROM "+trackingPlanVersionsTable+" WHERE tracking_plan_id")).
		WithArgs("tp-100", 99).
		WillReturnError(sql.ErrNoRows)

	_, err := repo.GetVersion(ctx, "tp-100", 99)
	require.ErrorIs(t, err, ErrVersionNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetVersion_DBError(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanVersionColumns)).
		WithArgs("tp-100", 1).
		WillReturnError(fmt.Errorf("network error"))

	_, err := repo.GetVersion(ctx, "tp-100", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "getting version")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Phase 11: JSON helper validation
// --------------------------------------------------------------------------

func TestValidateJSONBytes(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{name: "valid object", data: []byte(`{"key":"value"}`), wantErr: false},
		{name: "valid array", data: []byte(`[1,2,3]`), wantErr: false},
		{name: "valid string", data: []byte(`"hello"`), wantErr: false},
		{name: "valid number", data: []byte(`42`), wantErr: false},
		{name: "valid null", data: []byte(`null`), wantErr: false},
		{name: "valid boolean", data: []byte(`true`), wantErr: false},
		{name: "empty bytes", data: []byte{}, wantErr: false},
		{name: "nil bytes", data: nil, wantErr: false},
		{name: "broken JSON", data: []byte(`{invalid}`), wantErr: true},
		{name: "truncated JSON", data: []byte(`{"key":`), wantErr: true},
		{name: "plain text", data: []byte(`not json`), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateJSONBytes(tt.data, "test_field")
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "invalid JSON in test_field")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMarshalJSONField(t *testing.T) {
	t.Run("valid JSON is passed through", func(t *testing.T) {
		input := []byte(`{"type":"string"}`)
		result, err := marshalJSONField(input, "schema")
		require.NoError(t, err)
		require.NotNil(t, result)
		// Result should contain the original JSON (rawJSON marshal passes through)
		require.Contains(t, string(result), "type")
	})

	t.Run("empty input returns nil", func(t *testing.T) {
		result, err := marshalJSONField(nil, "schema")
		require.NoError(t, err)
		require.Nil(t, result)
	})

	t.Run("empty bytes returns nil", func(t *testing.T) {
		result, err := marshalJSONField([]byte{}, "schema")
		require.NoError(t, err)
		require.Nil(t, result)
	})
}

func TestRawJSON_MarshalJSON(t *testing.T) {
	t.Run("non-empty returns raw bytes", func(t *testing.T) {
		r := rawJSON(`{"key":"value"}`)
		b, err := r.MarshalJSON()
		require.NoError(t, err)
		require.Equal(t, `{"key":"value"}`, string(b))
	})

	t.Run("nil returns null", func(t *testing.T) {
		var r rawJSON
		b, err := r.MarshalJSON()
		require.NoError(t, err)
		require.Equal(t, "null", string(b))
	})

	t.Run("empty returns null", func(t *testing.T) {
		r := rawJSON([]byte{})
		b, err := r.MarshalJSON()
		require.NoError(t, err)
		require.Equal(t, "null", string(b))
	})
}

// --------------------------------------------------------------------------
// Phase 12: Sentinel errors
// --------------------------------------------------------------------------

func TestSentinelErrors(t *testing.T) {
	t.Run("ErrTrackingPlanNotFound is stable", func(t *testing.T) {
		require.True(t, errors.Is(ErrTrackingPlanNotFound, ErrTrackingPlanNotFound))
		require.Contains(t, ErrTrackingPlanNotFound.Error(), "tracking plan not found")
	})

	t.Run("ErrVersionNotFound is stable", func(t *testing.T) {
		require.True(t, errors.Is(ErrVersionNotFound, ErrVersionNotFound))
		require.Contains(t, ErrVersionNotFound.Error(), "tracking plan version not found")
	})

	t.Run("sentinel errors are distinct", func(t *testing.T) {
		require.False(t, errors.Is(ErrTrackingPlanNotFound, ErrVersionNotFound))
	})
}

// --------------------------------------------------------------------------
// Phase 13: Concurrent access
// --------------------------------------------------------------------------

func TestConcurrentAccess(t *testing.T) {
	// sqlmock processes expectations in FIFO order, so concurrent goroutines
	// with different arguments cannot use a single mock. Instead, we validate
	// thread safety by running multiple sequential operations from separate
	// goroutines that all share the same Repository instance. This confirms
	// that the underlying *sql.DB connection pool is safe for concurrent use.
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	const iterations = 10
	var wg sync.WaitGroup
	errs := make([]error, iterations)

	// All goroutines query the same ID sequentially, ensuring sqlmock's
	// FIFO expectations are consumed in order.
	for i := 0; i < iterations; i++ {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns)).
			WithArgs("tp-shared").
			WillReturnRows(
				sqlmock.NewRows(trackingPlanCols()).
					AddRow("tp-shared", "ws-001", "Shared Plan", nil, 1, nil, now, now),
			)
	}

	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = repo.Get(ctx, "tp-shared")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "concurrent Get(%d) should succeed", i)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Phase 14: Context cancellation
// --------------------------------------------------------------------------

func TestCreate_ContextCancelled(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	tp := TrackingPlan{
		WorkspaceID: "ws-001",
		Name:        "Plan",
		Version:     1,
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+trackingPlansTable)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(context.Canceled)

	_, err := repo.Create(ctx, tp)
	require.Error(t, err)
}

// --------------------------------------------------------------------------
// Phase 15: Full CRUD lifecycle
// --------------------------------------------------------------------------

func TestFullCRUDLifecycle(t *testing.T) {
	repo, mock, _ := newTestRepo(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	schemaJSON := sampleSchema()

	// Step 1: Create — use AnyArg for enforcement_config (position 4) because
	// marshalJSONField(nil) returns nil []byte, but the database/sql driver layer
	// may normalise nil []byte to empty []byte{} before reaching sqlmock.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+trackingPlansTable)).
		WithArgs("ws-lifecycle", "Lifecycle Plan", sqlmock.AnyArg(), 1, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tp-lifecycle"))

	id, err := repo.Create(ctx, TrackingPlan{
		WorkspaceID: "ws-lifecycle",
		Name:        "Lifecycle Plan",
		Schema:      schemaJSON,
		Version:     1,
	})
	require.NoError(t, err)
	require.Equal(t, "tp-lifecycle", id)

	// Step 2: Get
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns)).
		WithArgs("tp-lifecycle").
		WillReturnRows(
			sqlmock.NewRows(trackingPlanCols()).
				AddRow("tp-lifecycle", "ws-lifecycle", "Lifecycle Plan", string(schemaJSON), 1, nil, now, now),
		)

	tp, err := repo.Get(ctx, "tp-lifecycle")
	require.NoError(t, err)
	require.Equal(t, "Lifecycle Plan", tp.Name)

	// Step 3: Update — AnyArg for enforcement_config (position 3), same
	// nil-vs-empty normalisation issue as Create.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE "+trackingPlansTable)).
		WithArgs("Updated Lifecycle", sqlmock.AnyArg(), 2, sqlmock.AnyArg(), sqlmock.AnyArg(), "tp-lifecycle", "ws-lifecycle").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = repo.Update(ctx, TrackingPlan{
		ID:          "tp-lifecycle",
		WorkspaceID: "ws-lifecycle",
		Name:        "Updated Lifecycle",
		Schema:      schemaJSON,
		Version:     2,
	})
	require.NoError(t, err)

	// Step 4: Create version snapshot
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+trackingPlanVersionsTable)).
		WithArgs("tp-lifecycle", 2, sqlmock.AnyArg(), "Updated event types", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ver-lc"))

	verID, err := repo.CreateVersion(ctx, TrackingPlanVersion{
		TrackingPlanID: "tp-lifecycle",
		Version:        2,
		Schema:         schemaJSON,
		Changelog:      "Updated event types",
	})
	require.NoError(t, err)
	require.Equal(t, "ver-lc", verID)

	// Step 5: List versions
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanVersionColumns)).
		WithArgs("tp-lifecycle").
		WillReturnRows(
			sqlmock.NewRows(trackingPlanVersionCols()).
				AddRow("ver-lc", "tp-lifecycle", 2, string(schemaJSON), "Updated event types", now),
		)

	versions, err := repo.GetVersions(ctx, "tp-lifecycle")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	require.Equal(t, 2, versions[0].Version)

	// Step 6: Delete
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM "+trackingPlansTable)).
		WithArgs("tp-lifecycle", "ws-lifecycle").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = repo.Delete(ctx, "ws-lifecycle", "tp-lifecycle")
	require.NoError(t, err)

	// Step 7: Confirm deleted
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+trackingPlanColumns)).
		WithArgs("tp-lifecycle").
		WillReturnError(sql.ErrNoRows)

	_, err = repo.Get(ctx, "tp-lifecycle")
	require.ErrorIs(t, err, ErrTrackingPlanNotFound)

	require.NoError(t, mock.ExpectationsWereMet())
}
