// Package storage tests validate PostgreSQL persistence for user-defined functions.
//
// This file provides comprehensive unit tests for the functions Repository using
// go-sqlmock, covering CRUD operations (Create, Get, Update, Delete, List,
// GetByVersion), workspace isolation via multi-tenant scoping, nullable Settings
// JSON handling, version auto-increment, pagination, type filtering, and error
// handling. Tests follow the protocols/storage/repository_test.go sqlmock pattern.
package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
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

// newTestRepo creates a Repository backed by go-sqlmock with regexp matching.
// Uses logger.NOP to suppress log output during testing, following the same
// pattern as protocols/storage/repository_test.go.
func newTestRepo(t *testing.T) (*Repository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return New(db, logger.NOP), mock
}

// fnCols returns the column names matching functionsColumns constant order.
// Must match the Scan() order in scanFunction: id, workspace_id, name, type,
// code, version, settings, created_at, updated_at.
func fnCols() []string {
	return []string{"id", "workspace_id", "name", "type", "code", "version", "settings", "created_at", "updated_at"}
}

// sampleSettings returns a valid JSON settings payload for testing.
func sampleSettings() json.RawMessage {
	return json.RawMessage(`{"apiKey":"sk-test","region":"us-east-1"}`)
}

// fixedTime returns a fixed time for deterministic assertions.
func fixedTime() time.Time {
	return time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC)
}

// --------------------------------------------------------------------------
// Constructor Tests
// --------------------------------------------------------------------------

func TestNew(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := New(db, logger.NOP)
	require.NotNil(t, repo)
	require.NotNil(t, repo.db)
	require.NotNil(t, repo.log)
}

// --------------------------------------------------------------------------
// Create Tests
// --------------------------------------------------------------------------

func TestCreate_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	fn := &Function{
		WorkspaceID: "ws-001",
		Name:        "Test Source Function",
		Type:        "source",
		Code:        `async function onRequest(req, settings) { return []; }`,
		Settings:    sampleSettings(),
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+functionsTableName)).
		WithArgs(
			fn.WorkspaceID, fn.Name, fn.Type, fn.Code,
			1,                // version is set to 1 by Create
			sqlmock.AnyArg(), // settings bytes
			sqlmock.AnyArg(), // created_at
			sqlmock.AnyArg(), // updated_at
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))

	err := repo.Create(ctx, fn)
	require.NoError(t, err)
	require.Equal(t, "42", fn.ID)
	require.Equal(t, 1, fn.Version)
	require.False(t, fn.CreatedAt.IsZero())
	require.False(t, fn.UpdatedAt.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreate_NilSettings(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	fn := &Function{
		WorkspaceID: "ws-001",
		Name:        "No Settings Function",
		Type:        "destination",
		Code:        `async function onTrack(event, settings) { return {}; }`,
		Settings:    nil, // intentionally nil
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+functionsTableName)).
		WithArgs(
			fn.WorkspaceID, fn.Name, fn.Type, fn.Code,
			1,
			nil,              // nil settings passed as nil to DB
			sqlmock.AnyArg(), // created_at
			sqlmock.AnyArg(), // updated_at
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(43)))

	err := repo.Create(ctx, fn)
	require.NoError(t, err)
	require.Equal(t, "43", fn.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreate_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	fn := &Function{
		WorkspaceID: "ws-001",
		Name:        "Error Function",
		Type:        "source",
		Code:        `function onRequest() {}`,
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+functionsTableName)).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnError(errors.New("unique constraint violation"))

	err := repo.Create(ctx, fn)
	require.Error(t, err)
	require.Contains(t, err.Error(), "creating function")
	require.Contains(t, err.Error(), "unique constraint violation")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Get Tests
// --------------------------------------------------------------------------

func TestGet_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("42", "ws-001").
		WillReturnRows(
			sqlmock.NewRows(fnCols()).
				AddRow("42", "ws-001", "Test Source Function", "source",
					`async function onRequest() {}`, 1,
					`{"apiKey":"sk-test"}`, now, now),
		)

	fn, err := repo.Get(ctx, "42", "ws-001")
	require.NoError(t, err)
	require.NotNil(t, fn)
	require.Equal(t, "42", fn.ID)
	require.Equal(t, "ws-001", fn.WorkspaceID)
	require.Equal(t, "Test Source Function", fn.Name)
	require.Equal(t, "source", fn.Type)
	require.Equal(t, 1, fn.Version)
	require.JSONEq(t, `{"apiKey":"sk-test"}`, string(fn.Settings))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGet_NotFound(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("999", "ws-001").
		WillReturnError(sql.ErrNoRows)

	fn, err := repo.Get(ctx, "999", "ws-001")
	require.Error(t, err)
	require.Nil(t, fn)
	require.ErrorIs(t, err, ErrFunctionNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGet_WrongWorkspace(t *testing.T) {
	// Verifies multi-tenant isolation: request with wrong workspace returns not found.
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("42", "ws-wrong").
		WillReturnError(sql.ErrNoRows)

	fn, err := repo.Get(ctx, "42", "ws-wrong")
	require.Error(t, err)
	require.Nil(t, fn)
	require.ErrorIs(t, err, ErrFunctionNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGet_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("42", "ws-001").
		WillReturnError(errors.New("connection refused"))

	fn, err := repo.Get(ctx, "42", "ws-001")
	require.Error(t, err)
	require.Nil(t, fn)
	require.Contains(t, err.Error(), "getting function 42")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGet_NullSettings(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("44", "ws-001").
		WillReturnRows(
			sqlmock.NewRows(fnCols()).
				AddRow("44", "ws-001", "No Settings", "insert",
					`function onEvent() {}`, 1,
					nil, now, now), // NULL settings
		)

	fn, err := repo.Get(ctx, "44", "ws-001")
	require.NoError(t, err)
	require.NotNil(t, fn)
	require.Nil(t, fn.Settings) // NULL settings should remain nil
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Update Tests
// --------------------------------------------------------------------------

func TestUpdate_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	fn := &Function{
		ID:          "42",
		WorkspaceID: "ws-001",
		Name:        "Updated Function",
		Type:        "source",
		Code:        `async function onRequest() { return []; }`,
		Settings:    sampleSettings(),
	}

	mock.ExpectQuery(regexp.QuoteMeta("UPDATE "+functionsTableName)).
		WithArgs(
			fn.Name, fn.Type, fn.Code,
			sqlmock.AnyArg(), // settings bytes
			sqlmock.AnyArg(), // updated_at
			fn.ID, fn.WorkspaceID,
		).
		WillReturnRows(sqlmock.NewRows([]string{"version", "updated_at"}).AddRow(2, now))

	err := repo.Update(ctx, fn)
	require.NoError(t, err)
	require.Equal(t, 2, fn.Version)
	require.Equal(t, now, fn.UpdatedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdate_NotFound(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	fn := &Function{
		ID:          "999",
		WorkspaceID: "ws-001",
		Name:        "Ghost Function",
		Type:        "source",
		Code:        `function onRequest() {}`,
	}

	mock.ExpectQuery(regexp.QuoteMeta("UPDATE "+functionsTableName)).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnError(sql.ErrNoRows)

	err := repo.Update(ctx, fn)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrFunctionNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdate_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	fn := &Function{
		ID:          "42",
		WorkspaceID: "ws-001",
		Name:        "Error Function",
		Type:        "source",
		Code:        `function onRequest() {}`,
	}

	mock.ExpectQuery(regexp.QuoteMeta("UPDATE "+functionsTableName)).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnError(errors.New("deadlock detected"))

	err := repo.Update(ctx, fn)
	require.Error(t, err)
	require.Contains(t, err.Error(), "updating function 42")
	require.Contains(t, err.Error(), "deadlock detected")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdate_NilSettings(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	fn := &Function{
		ID:          "42",
		WorkspaceID: "ws-001",
		Name:        "Nil Settings Update",
		Type:        "insert",
		Code:        `function onEvent(e) { return e; }`,
		Settings:    nil, // nil settings
	}

	mock.ExpectQuery(regexp.QuoteMeta("UPDATE "+functionsTableName)).
		WithArgs(
			fn.Name, fn.Type, fn.Code,
			nil,              // nil settings
			sqlmock.AnyArg(), // updated_at
			fn.ID, fn.WorkspaceID,
		).
		WillReturnRows(sqlmock.NewRows([]string{"version", "updated_at"}).AddRow(3, now))

	err := repo.Update(ctx, fn)
	require.NoError(t, err)
	require.Equal(t, 3, fn.Version)
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Delete Tests
// --------------------------------------------------------------------------

func TestDelete_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM "+functionsTableName)).
		WithArgs("42", "ws-001").
		WillReturnResult(sqlmock.NewResult(0, 1)) // 1 row affected

	err := repo.Delete(ctx, "42", "ws-001")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDelete_NotFound(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM "+functionsTableName)).
		WithArgs("999", "ws-001").
		WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows affected

	err := repo.Delete(ctx, "999", "ws-001")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrFunctionNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDelete_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM "+functionsTableName)).
		WithArgs("42", "ws-001").
		WillReturnError(errors.New("foreign key violation"))

	err := repo.Delete(ctx, "42", "ws-001")
	require.Error(t, err)
	require.Contains(t, err.Error(), "deleting function 42")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDelete_RowsAffectedError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM "+functionsTableName)).
		WithArgs("42", "ws-001").
		WillReturnResult(sqlmock.NewErrorResult(errors.New("rows affected error")))

	err := repo.Delete(ctx, "42", "ws-001")
	require.Error(t, err)
	require.Contains(t, err.Error(), "getting rows affected")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// List Tests
// --------------------------------------------------------------------------

func TestList_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery("SELECT .+ FROM "+functionsTableName+" WHERE workspace_id").
		WithArgs("ws-001", defaultListLimit).
		WillReturnRows(
			sqlmock.NewRows(fnCols()).
				AddRow("42", "ws-001", "Source Fn", "source", "code1", 1, `{"k":"v"}`, now, now).
				AddRow("43", "ws-001", "Dest Fn", "destination", "code2", 2, nil, now, now),
		)

	fns, err := repo.List(ctx, "ws-001", ListOptions{})
	require.NoError(t, err)
	require.Len(t, fns, 2)
	require.Equal(t, "42", fns[0].ID)
	require.Equal(t, "Source Fn", fns[0].Name)
	require.Equal(t, "43", fns[1].ID)
	require.Equal(t, "Dest Fn", fns[1].Name)
	require.Nil(t, fns[1].Settings) // NULL settings
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_WithTypeFilter(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery("SELECT .+ FROM "+functionsTableName+" WHERE workspace_id .+ AND type").
		WithArgs("ws-001", "source", defaultListLimit).
		WillReturnRows(
			sqlmock.NewRows(fnCols()).
				AddRow("42", "ws-001", "Source Fn", "source", "code1", 1, nil, now, now),
		)

	fns, err := repo.List(ctx, "ws-001", ListOptions{TypeFilter: "source"})
	require.NoError(t, err)
	require.Len(t, fns, 1)
	require.Equal(t, "source", fns[0].Type)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_WithPagination(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery("SELECT .+ FROM "+functionsTableName+" WHERE workspace_id .+ LIMIT .+ OFFSET").
		WithArgs("ws-001", 10, 20).
		WillReturnRows(sqlmock.NewRows(fnCols()))

	fns, err := repo.List(ctx, "ws-001", ListOptions{Limit: 10, Offset: 20})
	require.NoError(t, err)
	require.NotNil(t, fns)
	require.Len(t, fns, 0) // empty result, but non-nil slice
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_EmptyResult(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery("SELECT .+ FROM "+functionsTableName+" WHERE workspace_id").
		WithArgs("ws-empty", defaultListLimit).
		WillReturnRows(sqlmock.NewRows(fnCols()))

	fns, err := repo.List(ctx, "ws-empty", ListOptions{})
	require.NoError(t, err)
	require.NotNil(t, fns, "List should return empty non-nil slice")
	require.Len(t, fns, 0)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery("SELECT .+ FROM "+functionsTableName+" WHERE workspace_id").
		WithArgs("ws-001", defaultListLimit).
		WillReturnError(errors.New("connection reset"))

	fns, err := repo.List(ctx, "ws-001", ListOptions{})
	require.Error(t, err)
	require.Nil(t, fns)
	require.Contains(t, err.Error(), "listing functions")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_WithTypeFilterAndPagination(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery("SELECT .+ FROM "+functionsTableName+" WHERE workspace_id .+ AND type .+ LIMIT .+ OFFSET").
		WithArgs("ws-001", "insert", 5, 10).
		WillReturnRows(
			sqlmock.NewRows(fnCols()).
				AddRow("50", "ws-001", "Insert Fn", "insert", "code", 1, nil, now, now),
		)

	fns, err := repo.List(ctx, "ws-001", ListOptions{TypeFilter: "insert", Limit: 5, Offset: 10})
	require.NoError(t, err)
	require.Len(t, fns, 1)
	require.Equal(t, "insert", fns[0].Type)
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// GetByVersion Tests
// --------------------------------------------------------------------------

func TestGetByVersion_Success(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("42", 2, "ws-001").
		WillReturnRows(
			sqlmock.NewRows(fnCols()).
				AddRow("42", "ws-001", "Source Fn v2", "source",
					`async function onRequest() { return []; }`, 2,
					`{"version":"2"}`, now, now),
		)

	fn, err := repo.GetByVersion(ctx, "42", 2, "ws-001")
	require.NoError(t, err)
	require.NotNil(t, fn)
	require.Equal(t, "42", fn.ID)
	require.Equal(t, 2, fn.Version)
	require.Equal(t, "Source Fn v2", fn.Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetByVersion_NotFound(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("42", 99, "ws-001").
		WillReturnError(sql.ErrNoRows)

	fn, err := repo.GetByVersion(ctx, "42", 99, "ws-001")
	require.Error(t, err)
	require.Nil(t, fn)
	require.ErrorIs(t, err, ErrFunctionNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetByVersion_DBError(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("42", 1, "ws-001").
		WillReturnError(errors.New("timeout exceeded"))

	fn, err := repo.GetByVersion(ctx, "42", 1, "ws-001")
	require.Error(t, err)
	require.Nil(t, fn)
	require.Contains(t, err.Error(), "getting function 42 version 1")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// scanFunction Tests (via Get with various settings states)
// --------------------------------------------------------------------------

func TestScanFunction_ValidSettingsJSON(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	settings := `{"nested":{"key":"value"},"array":[1,2,3]}`
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("45", "ws-001").
		WillReturnRows(
			sqlmock.NewRows(fnCols()).
				AddRow("45", "ws-001", "Complex Settings", "destination",
					"code", 1, settings, now, now),
		)

	fn, err := repo.Get(ctx, "45", "ws-001")
	require.NoError(t, err)
	require.NotNil(t, fn)
	require.JSONEq(t, settings, string(fn.Settings))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScanFunction_EmptyStringSettings(t *testing.T) {
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	now := fixedTime()
	// Empty string is a valid NullString{Valid: true, String: ""}.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT "+functionsColumns+" FROM "+functionsTableName)).
		WithArgs("46", "ws-001").
		WillReturnRows(
			sqlmock.NewRows(fnCols()).
				AddRow("46", "ws-001", "Empty Settings", "source",
					"code", 1, "", now, now),
		)

	fn, err := repo.Get(ctx, "46", "ws-001")
	require.NoError(t, err)
	require.NotNil(t, fn)
	// Empty string "" is a valid NullString so Settings will be json.RawMessage("")
	require.Equal(t, json.RawMessage(""), fn.Settings)
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Edge Case Tests
// --------------------------------------------------------------------------

func TestCreate_SetsVersionAndTimestamps(t *testing.T) {
	// Verify that Create always sets version=1 regardless of input,
	// and sets both CreatedAt and UpdatedAt timestamps.
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	fn := &Function{
		WorkspaceID: "ws-001",
		Name:        "Version Override Test",
		Type:        "source",
		Code:        "code",
		Version:     99, // should be overridden to 1
	}

	before := time.Now().Add(-time.Second)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO "+functionsTableName)).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			1, // version must be 1
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(100)))

	err := repo.Create(ctx, fn)
	require.NoError(t, err)
	after := time.Now().Add(time.Second)

	require.Equal(t, 1, fn.Version, "Create should force version=1")
	require.True(t, fn.CreatedAt.After(before), "CreatedAt should be set to ~now")
	require.True(t, fn.CreatedAt.Before(after), "CreatedAt should be set to ~now")
	require.True(t, fn.UpdatedAt.After(before), "UpdatedAt should be set to ~now")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_DefaultLimitApplied(t *testing.T) {
	// Verify that a zero Limit defaults to defaultListLimit (100).
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery("SELECT .+ FROM "+functionsTableName+" WHERE workspace_id .+ LIMIT").
		WithArgs("ws-001", defaultListLimit). // should be 100
		WillReturnRows(sqlmock.NewRows(fnCols()))

	_, err := repo.List(ctx, "ws-001", ListOptions{Limit: 0})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_NegativeLimitDefaultsToDefault(t *testing.T) {
	// Verify that a negative Limit defaults to defaultListLimit.
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	mock.ExpectQuery("SELECT .+ FROM "+functionsTableName+" WHERE workspace_id .+ LIMIT").
		WithArgs("ws-001", defaultListLimit).
		WillReturnRows(sqlmock.NewRows(fnCols()))

	_, err := repo.List(ctx, "ws-001", ListOptions{Limit: -5})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestList_ScanError(t *testing.T) {
	// Verify that a scan error during row iteration is propagated correctly.
	repo, mock := newTestRepo(t)
	ctx := context.Background()

	// Return a row with wrong number of columns to trigger a scan error.
	mock.ExpectQuery("SELECT .+ FROM "+functionsTableName+" WHERE workspace_id").
		WithArgs("ws-001", defaultListLimit).
		WillReturnRows(
			sqlmock.NewRows([]string{"id"}).AddRow("only-one-column"),
		)

	fns, err := repo.List(ctx, "ws-001", ListOptions{})
	require.Error(t, err)
	require.Nil(t, fns)
	require.Contains(t, err.Error(), "scanning function row")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --------------------------------------------------------------------------
// Sentinel Error Tests
// --------------------------------------------------------------------------

func TestErrFunctionNotFound_Sentinel(t *testing.T) {
	// Verify ErrFunctionNotFound can be checked with errors.Is.
	wrapped := fmt.Errorf("wrapped: %w", ErrFunctionNotFound)
	require.True(t, errors.Is(wrapped, ErrFunctionNotFound))
}

// --------------------------------------------------------------------------
// sqlmock driver.Result implementation for RowsAffected error test
// --------------------------------------------------------------------------

// Suppress unused import lint — driver is used by the test above via
// sqlmock.NewErrorResult which returns driver.Result.
var _ driver.Result = sqlmock.NewErrorResult(nil)
