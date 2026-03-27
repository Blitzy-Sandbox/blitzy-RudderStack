package router

import (
	"testing"

	"github.com/rudderlabs/rudder-server/warehouse/internal/model"

	"github.com/stretchr/testify/require"
)

func TestState(t *testing.T) {
	t.Run("inProgressState", func(t *testing.T) {
		t.Run("invalid state", func(t *testing.T) {
			require.Panics(t, func() {
				inProgressState("unknown")
			})
		})
		t.Run("valid state", func(t *testing.T) {
			testcases := []struct {
				current         string
				inProgressState string
			}{
				{current: model.GeneratedUploadSchema, inProgressState: "generating_upload_schema"},
				{current: model.CreatedTableUploads, inProgressState: "creating_table_uploads"},
				{current: model.GeneratedLoadFiles, inProgressState: "generating_load_files"},
				{current: model.UpdatedTableUploadsCounts, inProgressState: "updating_table_uploads_counts"},
				{current: model.CreatedRemoteSchema, inProgressState: "creating_remote_schema"},
				{current: model.ExportedData, inProgressState: "exporting_data"},
			}

			for index, tc := range testcases {
				require.Equal(t, tc.inProgressState, inProgressState(tc.current), "test case %d", index)
			}
		})
	})
	t.Run("nextState", func(t *testing.T) {
		testCases := []struct {
			current string
			next    *state
		}{
			{current: "unknown", next: nil},

			// completed states
			{current: model.Waiting, next: stateTransitions[model.GeneratedUploadSchema]},
			{current: model.GeneratedUploadSchema, next: stateTransitions[model.CreatedTableUploads]},
			{current: model.CreatedTableUploads, next: stateTransitions[model.GeneratedLoadFiles]},
			{current: model.GeneratedLoadFiles, next: stateTransitions[model.UpdatedTableUploadsCounts]},
			{current: model.UpdatedTableUploadsCounts, next: stateTransitions[model.CreatedRemoteSchema]},
			{current: model.CreatedRemoteSchema, next: stateTransitions[model.ExportedData]},
			{current: model.ExportedData, next: nil},
			{current: model.Aborted, next: nil},

			// in progress states
			{current: "generating_upload_schema", next: stateTransitions[model.GeneratedUploadSchema]},
			{current: "creating_table_uploads", next: stateTransitions[model.CreatedTableUploads]},
			{current: "generating_load_files", next: stateTransitions[model.GeneratedLoadFiles]},
			{current: "updating_table_uploads_counts", next: stateTransitions[model.UpdatedTableUploadsCounts]},
			{current: "creating_remote_schema", next: stateTransitions[model.CreatedRemoteSchema]},
			{current: "exporting_data", next: stateTransitions[model.ExportedData]},

			// failed states
			{current: "generating_upload_schema_failed", next: stateTransitions[model.GeneratedUploadSchema]},
			{current: "creating_table_uploads_failed", next: stateTransitions[model.CreatedTableUploads]},
			{current: "generating_load_files_failed", next: stateTransitions[model.GeneratedLoadFiles]},
			{current: "updating_table_uploads_counts_failed", next: stateTransitions[model.UpdatedTableUploadsCounts]},
			{current: "creating_remote_schema_failed", next: stateTransitions[model.CreatedRemoteSchema]},
			{current: "exporting_data_failed", next: stateTransitions[model.ExportedData]},
		}
		for index, tc := range testCases {
			require.Equal(t, tc.next, nextState(tc.current), "test case %d", index)
		}
	})
}

// TestBackfillStateTransitions validates backfill state machine extensions introduced
// by E-032 (Configurable Backfill). The backfill state is a parallel entry point for
// uploads with a non-nil BackfillJobID; it transitions into the standard
// GeneratedUploadSchema → ExportedData chain after backfill resolution completes.
func TestBackfillStateTransitions(t *testing.T) {
	t.Run("backfill state registered in stateTransitions", func(t *testing.T) {
		// Verify that BackfillPending exists as a key in the state machine transition map
		// and carries the correct metadata fields.
		st, ok := stateTransitions[model.BackfillPending]
		require.True(t, ok, "BackfillPending should be registered in stateTransitions")
		require.NotNil(t, st, "backfill state entry should not be nil")
		require.Equal(t, model.BackfillPending, st.completed,
			"backfill completed state should be BackfillPending")
		require.Equal(t, model.BackfillInProgress, st.inProgress,
			"backfill in-progress state should be BackfillInProgress")
		require.Equal(t, model.BackfillPending, st.failed,
			"backfill failed state should be BackfillPending")
	})

	t.Run("inProgressState for backfill states", func(t *testing.T) {
		// inProgressState for BackfillPending should return BackfillInProgress.
		got := inProgressState(model.BackfillPending)
		require.Equal(t, model.BackfillInProgress, got,
			"inProgressState(BackfillPending) should return BackfillInProgress")
	})

	t.Run("nextState for backfill states", func(t *testing.T) {
		// BackfillPending (completed) → GeneratedUploadSchema state.
		// After backfill resolution succeeds the upload enters the normal schema flow.
		nextFromBackfill := nextState(model.BackfillPending)
		require.NotNil(t, nextFromBackfill,
			"nextState(BackfillPending) should not be nil")
		require.Equal(t, stateTransitions[model.GeneratedUploadSchema], nextFromBackfill,
			"after backfill completion, next state should be GeneratedUploadSchema")

		// BackfillInProgress (in-progress string) resolves back to the
		// BackfillPending state entry, consistent with how other in-progress
		// states resolve to their parent state.
		nextFromInProgress := nextState(model.BackfillInProgress)
		require.NotNil(t, nextFromInProgress,
			"nextState(BackfillInProgress) should not be nil")
		require.Equal(t, stateTransitions[model.BackfillPending], nextFromInProgress,
			"backfill in-progress should resolve to the BackfillPending state")
	})

	t.Run("regular uploads bypass backfill state", func(t *testing.T) {
		// The standard chain Waiting → GeneratedUploadSchema must remain intact.
		// Regular uploads (BackfillJobID == nil) never enter the backfill state.
		nextFromWaiting := nextState(model.Waiting)
		require.NotNil(t, nextFromWaiting,
			"nextState(Waiting) should not be nil")
		require.Equal(t, stateTransitions[model.GeneratedUploadSchema], nextFromWaiting,
			"regular uploads should go from Waiting directly to GeneratedUploadSchema, bypassing backfill")
		require.NotEqual(t, stateTransitions[model.BackfillPending], nextFromWaiting,
			"Waiting must NOT transition to BackfillPending for regular uploads")
	})

	t.Run("backward compatibility — all existing state transitions preserved", func(t *testing.T) {
		// Walk the complete original chain:
		// Waiting → GeneratedUploadSchema → CreatedTableUploads → GeneratedLoadFiles
		// → UpdatedTableUploadsCounts → CreatedRemoteSchema → ExportedData (terminal)
		expectedChain := []string{
			model.Waiting,
			model.GeneratedUploadSchema,
			model.CreatedTableUploads,
			model.GeneratedLoadFiles,
			model.UpdatedTableUploadsCounts,
			model.CreatedRemoteSchema,
			model.ExportedData,
		}
		for i := 0; i < len(expectedChain)-1; i++ {
			current := expectedChain[i]
			next := nextState(current)
			require.NotNil(t, next, "nextState(%q) should not be nil", current)
			require.Equal(t, stateTransitions[expectedChain[i+1]], next,
				"from %q, next should be %q", current, expectedChain[i+1])
		}

		// ExportedData is terminal — nextState returns nil.
		require.Nil(t, nextState(model.ExportedData),
			"ExportedData should be terminal (next is nil)")

		// Aborted is also terminal — nextState returns nil.
		require.Nil(t, nextState(model.Aborted),
			"Aborted should be terminal (next is nil)")
	})
}

// TestSelectiveSyncStateAwareness validates that the state machine transitions are
// not altered by the selective sync feature (E-034). Selective sync filtering occurs
// within individual state handler functions (state_generate_load_files.go,
// state_export_data.go, state_create_table_uploads.go), not in the state machine
// structure itself. These tests confirm structural invariance.
func TestSelectiveSyncStateAwareness(t *testing.T) {
	t.Run("state transitions remain unchanged regardless of selective sync config", func(t *testing.T) {
		// All original states must still exist in the transition map.
		// Selective sync does not add, remove, or reorder any state.
		originalStates := []string{
			model.Waiting,
			model.GeneratedUploadSchema,
			model.CreatedTableUploads,
			model.GeneratedLoadFiles,
			model.UpdatedTableUploadsCounts,
			model.CreatedRemoteSchema,
			model.ExportedData,
			model.Aborted,
		}
		for _, s := range originalStates {
			_, ok := stateTransitions[s]
			require.True(t, ok, "state %q must remain registered in stateTransitions", s)
		}

		// Each state's inProgress and failed strings must be unchanged.
		expectedInProgress := map[string]string{
			model.GeneratedUploadSchema:     "generating_upload_schema",
			model.CreatedTableUploads:       "creating_table_uploads",
			model.GeneratedLoadFiles:        "generating_load_files",
			model.UpdatedTableUploadsCounts: "updating_table_uploads_counts",
			model.CreatedRemoteSchema:       "creating_remote_schema",
			model.ExportedData:              "exporting_data",
		}
		for completed, expectedIP := range expectedInProgress {
			st := stateTransitions[completed]
			require.Equal(t, expectedIP, st.inProgress,
				"inProgress for %q should be %q", completed, expectedIP)
		}

		expectedFailed := map[string]string{
			model.GeneratedUploadSchema:     "generating_upload_schema_failed",
			model.CreatedTableUploads:       "creating_table_uploads_failed",
			model.GeneratedLoadFiles:        "generating_load_files_failed",
			model.UpdatedTableUploadsCounts: "updating_table_uploads_counts_failed",
			model.CreatedRemoteSchema:       "creating_remote_schema_failed",
			model.ExportedData:              "exporting_data_failed",
		}
		for completed, expectedF := range expectedFailed {
			st := stateTransitions[completed]
			require.Equal(t, expectedF, st.failed,
				"failed for %q should be %q", completed, expectedF)
		}
	})

	t.Run("complete state chain with selective sync enabled", func(t *testing.T) {
		// Even when selective sync is enabled, the state machine walks the same chain
		// from Waiting through ExportedData. No state is skipped or inserted.
		chain := []string{
			model.Waiting,
			model.GeneratedUploadSchema,
			model.CreatedTableUploads,
			model.GeneratedLoadFiles,
			model.UpdatedTableUploadsCounts,
			model.CreatedRemoteSchema,
			model.ExportedData,
		}

		current := chain[0]
		for i := 1; i < len(chain); i++ {
			next := nextState(current)
			require.NotNil(t, next,
				"nextState(%q) should not be nil at step %d", current, i)
			require.Equal(t, chain[i], next.completed,
				"at step %d, expected next completed state to be %q, got %q",
				i, chain[i], next.completed)
			current = next.completed
		}

		// Final state should be terminal — nextState returns nil.
		finalNext := nextState(current)
		require.Nil(t, finalNext,
			"ExportedData should be terminal — nextState should return nil")
	})
}
