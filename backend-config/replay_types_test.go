package backendconfig

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyReplayConfig(t *testing.T) {
	t.Run("Valid Replay Config", func(t *testing.T) {
		c := &ConfigT{
			Sources: []SourceT{
				{
					ID:     "s-1",
					Config: json.RawMessage(`{"eventUpload": true}`),
					SourceDefinition: SourceDefinitionT{
						ID:       "sd-1",
						Type:     "type-1",
						Category: "category-1",
					},
					Destinations: []DestinationT{
						{
							ID:                 "d-1",
							RevisionID:         "rev-1",
							IsProcessorEnabled: false,
						},
						{
							ID:         "d-2",
							RevisionID: "rev-2",
						},
					},
				},
			},
			EventReplays: map[string]EventReplayConfig{
				"er-1": {
					Sources: map[string]EventReplaySource{
						"er-s-1": {
							OriginalSourceID: "s-1",
						},
					},
					Destinations: map[string]EventReplayDestination{
						"er-d-1": {
							OriginalDestinationID: "d-1",
						},
					},
					Connections: []EventReplayConnection{
						{
							SourceID:      "er-s-1",
							DestinationID: "er-d-1",
						},
					},
				},
			},
		}
		c.ApplyReplaySources()

		require.Len(t, c.Sources, 2)
		require.Equal(t, "s-1", c.Sources[0].ID)
		require.Equal(t, "er-s-1", c.Sources[1].ID)
		require.Equal(t, "s-1", c.Sources[1].OriginalID)
		require.Equal(t, "er-s-1", c.Sources[1].WriteKey)
		require.JSONEq(t, "{}", string(c.Sources[1].Config))
		require.Len(t, c.Sources[1].Destinations, 1)
		require.Equal(t, "er-d-1", c.Sources[1].Destinations[0].ID)
		require.Equal(t, true, c.Sources[1].Destinations[0].IsProcessorEnabled)
		require.Equal(t, "rev-1", c.Sources[1].Destinations[0].RevisionID)
	})

	t.Run("Invalid Replay Config", func(t *testing.T) {
		c := &ConfigT{
			Sources: []SourceT{
				{
					ID:     "s-1",
					Config: json.RawMessage(`{"eventUpload": true}`),
					SourceDefinition: SourceDefinitionT{
						ID:       "sd-1",
						Type:     "type-1",
						Category: "category-1",
					},
					Destinations: []DestinationT{
						{
							ID: "d-1",
						},
					},
				},
			},
			EventReplays: map[string]EventReplayConfig{
				"er-1": {
					Sources: map[string]EventReplaySource{
						"er-s-1": {
							OriginalSourceID: "s-1",
						},
						"er-s-2": {
							OriginalSourceID: "s-2",
						},
					},
					Destinations: map[string]EventReplayDestination{
						"er-d-1": {
							OriginalDestinationID: "d-1",
						},
						"er-d-2": {
							OriginalDestinationID: "d-2",
						},
					},
					Connections: []EventReplayConnection{
						{
							SourceID:      "er-s-1",
							DestinationID: "er-d-1",
						},
						{
							SourceID:      "er-s-1",
							DestinationID: "er-d-2",
						},
						{
							SourceID:      "er-s-2",
							DestinationID: "er-d-1",
						},
						{
							SourceID:      "er-s-2",
							DestinationID: "er-d-2",
						},
						{
							SourceID:      "er-s-3",
							DestinationID: "er-d-3",
						},
					},
				},
			},
		}

		c.ApplyReplaySources()

		require.Len(t, c.Sources, 2)
		require.Equal(t, "s-1", c.Sources[0].ID)
		require.Equal(t, "er-s-1", c.Sources[1].ID)
		require.Equal(t, "s-1", c.Sources[1].OriginalID)
		require.Equal(t, "er-s-1", c.Sources[1].WriteKey)
		require.JSONEq(t, "{}", string(c.Sources[1].Config))
		require.Len(t, c.Sources[1].Destinations, 1)
		require.Equal(t, "er-d-1", c.Sources[1].Destinations[0].ID)
	})

	t.Run("WarehouseOnly Replay Config", func(t *testing.T) {
		c := &ConfigT{
			Sources: []SourceT{
				{
					ID:     "s-1",
					Config: json.RawMessage(`{"eventUpload": true}`),
					SourceDefinition: SourceDefinitionT{
						ID:       "sd-1",
						Type:     "type-1",
						Category: "category-1",
					},
					Destinations: []DestinationT{
						{
							ID:                 "d-1",
							RevisionID:         "rev-1",
							IsProcessorEnabled: false,
						},
					},
				},
			},
			EventReplays: map[string]EventReplayConfig{
				"er-1": {
					WarehouseOnly: true,
					Sources: map[string]EventReplaySource{
						"er-s-1": {
							OriginalSourceID: "s-1",
						},
					},
					Destinations: map[string]EventReplayDestination{
						"er-d-1": {
							OriginalDestinationID: "d-1",
						},
					},
					Connections: []EventReplayConnection{
						{
							SourceID:      "er-s-1",
							DestinationID: "er-d-1",
						},
					},
				},
			},
		}
		c.ApplyReplaySources()

		require.Len(t, c.Sources, 2)
		require.Equal(t, "s-1", c.Sources[0].ID)
		require.Equal(t, "er-s-1", c.Sources[1].ID)
		require.Equal(t, "s-1", c.Sources[1].OriginalID)
		require.Equal(t, "er-s-1", c.Sources[1].WriteKey)
		// When WarehouseOnly is true, ApplyReplaySources injects "warehouseOnly":true into the source config
		require.JSONEq(t, `{"warehouseOnly":true}`, string(c.Sources[1].Config))
		require.Len(t, c.Sources[1].Destinations, 1)
		require.Equal(t, "er-d-1", c.Sources[1].Destinations[0].ID)
		require.Equal(t, true, c.Sources[1].Destinations[0].IsProcessorEnabled)
		require.Equal(t, "rev-1", c.Sources[1].Destinations[0].RevisionID)

		// Verify WarehouseOnly flag is accessible on the replay config
		replayConfig, ok := c.EventReplays["er-1"]
		require.True(t, ok)
		require.True(t, replayConfig.WarehouseOnly)
	})

	t.Run("WarehouseOnly Default False", func(t *testing.T) {
		c := &ConfigT{
			Sources: []SourceT{
				{
					ID:     "s-1",
					Config: json.RawMessage(`{"eventUpload": true}`),
					SourceDefinition: SourceDefinitionT{
						ID:       "sd-1",
						Type:     "type-1",
						Category: "category-1",
					},
					Destinations: []DestinationT{
						{
							ID:                 "d-1",
							IsProcessorEnabled: false,
						},
					},
				},
			},
			EventReplays: map[string]EventReplayConfig{
				"er-1": {
					// WarehouseOnly NOT set — should default to false
					Sources: map[string]EventReplaySource{
						"er-s-1": {
							OriginalSourceID: "s-1",
						},
					},
					Destinations: map[string]EventReplayDestination{
						"er-d-1": {
							OriginalDestinationID: "d-1",
						},
					},
					Connections: []EventReplayConnection{
						{
							SourceID:      "er-s-1",
							DestinationID: "er-d-1",
						},
					},
				},
			},
		}
		c.ApplyReplaySources()

		require.Len(t, c.Sources, 2)
		require.Equal(t, "er-s-1", c.Sources[1].ID)

		// Verify WarehouseOnly defaults to false (backward compatibility)
		replayConfig, ok := c.EventReplays["er-1"]
		require.True(t, ok)
		require.False(t, replayConfig.WarehouseOnly)
	})

	t.Run("Multiple Replays Mixed WarehouseOnly", func(t *testing.T) {
		c := &ConfigT{
			Sources: []SourceT{
				{
					ID:     "s-1",
					Config: json.RawMessage(`{}`),
					Destinations: []DestinationT{
						{ID: "d-1", IsProcessorEnabled: false},
					},
				},
			},
			EventReplays: map[string]EventReplayConfig{
				"er-warehouse": {
					WarehouseOnly: true,
					Sources: map[string]EventReplaySource{
						"er-s-wh": {OriginalSourceID: "s-1"},
					},
					Destinations: map[string]EventReplayDestination{
						"er-d-wh": {OriginalDestinationID: "d-1"},
					},
					Connections: []EventReplayConnection{
						{SourceID: "er-s-wh", DestinationID: "er-d-wh"},
					},
				},
				"er-regular": {
					WarehouseOnly: false,
					Sources: map[string]EventReplaySource{
						"er-s-reg": {OriginalSourceID: "s-1"},
					},
					Destinations: map[string]EventReplayDestination{
						"er-d-reg": {OriginalDestinationID: "d-1"},
					},
					Connections: []EventReplayConnection{
						{SourceID: "er-s-reg", DestinationID: "er-d-reg"},
					},
				},
			},
		}
		c.ApplyReplaySources()

		// Both replay configs should produce sources (original + 2 replay sources)
		require.Len(t, c.Sources, 3)

		// Verify both WarehouseOnly flags preserved
		whConfig, ok := c.EventReplays["er-warehouse"]
		require.True(t, ok)
		require.True(t, whConfig.WarehouseOnly)

		regConfig, ok := c.EventReplays["er-regular"]
		require.True(t, ok)
		require.False(t, regConfig.WarehouseOnly)
	})
}
