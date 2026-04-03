package enforcement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-server/processor/enforcement"
)

// ---------------------------------------------------------------------------
// Phase 1: Test Mode Constants
// ---------------------------------------------------------------------------

// TestMode_Constants verifies that the three enforcement mode constants have
// the expected string values that must match the JSON field values used in
// backend-config/types.go TrackingPlanEnforcementConfig.
func TestMode_Constants(t *testing.T) {
	assert.Equal(t, enforcement.Mode("block"), enforcement.ModeBlock)
	assert.Equal(t, enforcement.Mode("omit"), enforcement.ModeOmit)
	assert.Equal(t, enforcement.Mode("allow"), enforcement.ModeAllow)
}

// TestMode_ConstantUniqueness verifies that all three mode constants are
// distinct from each other, preventing accidental duplication.
func TestMode_ConstantUniqueness(t *testing.T) {
	assert.NotEqual(t, enforcement.ModeBlock, enforcement.ModeOmit)
	assert.NotEqual(t, enforcement.ModeBlock, enforcement.ModeAllow)
	assert.NotEqual(t, enforcement.ModeOmit, enforcement.ModeAllow)
}

// TestMode_StringRepresentation verifies that casting Mode to string returns
// the expected lowercase string value.
func TestMode_StringRepresentation(t *testing.T) {
	require.Equal(t, "block", string(enforcement.ModeBlock))
	require.Equal(t, "omit", string(enforcement.ModeOmit))
	require.Equal(t, "allow", string(enforcement.ModeAllow))
}

// ---------------------------------------------------------------------------
// Phase 2: Test Mode Validation (IsValidMode)
// ---------------------------------------------------------------------------

// TestIsValidMode_ValidModes verifies that each of the three enforcement mode
// constants is recognized as valid by the IsValidMode function.
func TestIsValidMode_ValidModes(t *testing.T) {
	assert.True(t, enforcement.IsValidMode(enforcement.ModeBlock))
	assert.True(t, enforcement.IsValidMode(enforcement.ModeOmit))
	assert.True(t, enforcement.IsValidMode(enforcement.ModeAllow))
}

// TestIsValidMode_InvalidModes verifies that invalid, empty, or improperly
// cased strings are not recognized as valid enforcement modes.
func TestIsValidMode_InvalidModes(t *testing.T) {
	tests := []struct {
		name string
		mode enforcement.Mode
	}{
		{"empty string", ""},
		{"arbitrary invalid", "invalid"},
		{"uppercase BLOCK", "BLOCK"},
		{"title case Block", "Block"},
		{"unrelated drop", "drop"},
		{"whitespace", " block "},
		{"partial", "blo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, enforcement.IsValidMode(tc.mode),
				"mode %q should be invalid", tc.mode)
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 3: Test Mode Resolution (ResolveMode — Per-Call-Type Override)
// ---------------------------------------------------------------------------

// TestResolveMode_GlobalOnly verifies that when only a global mode is set
// (no per-call-type overrides), the global mode is returned for any event type.
func TestResolveMode_GlobalOnly(t *testing.T) {
	assert.Equal(t, enforcement.ModeBlock,
		enforcement.ResolveMode(enforcement.ModeBlock, nil, "track"))
	assert.Equal(t, enforcement.ModeOmit,
		enforcement.ResolveMode(enforcement.ModeOmit, nil, "identify"))
	assert.Equal(t, enforcement.ModeAllow,
		enforcement.ResolveMode(enforcement.ModeAllow, nil, "page"))
}

// TestResolveMode_PerCallTypeOverride verifies that a per-call-type override
// takes precedence over the global mode for the matching event type, while
// other event types still fall back to the global mode.
func TestResolveMode_PerCallTypeOverride(t *testing.T) {
	overrides := map[string]enforcement.Mode{
		"track": enforcement.ModeBlock,
	}

	// Override applied for matching event type
	assert.Equal(t, enforcement.ModeBlock,
		enforcement.ResolveMode(enforcement.ModeAllow, overrides, "track"))

	// No override for identify — falls back to global
	assert.Equal(t, enforcement.ModeAllow,
		enforcement.ResolveMode(enforcement.ModeAllow, overrides, "identify"))
}

// TestResolveMode_AllCallTypes verifies that per-call-type overrides work
// correctly for ALL five supported event types: track, identify, group, page,
// and screen. This ensures exhaustive coverage per Rule 0.7.2.
func TestResolveMode_AllCallTypes(t *testing.T) {
	for _, callType := range enforcement.SupportedCallTypes {
		t.Run(callType, func(t *testing.T) {
			overrides := map[string]enforcement.Mode{
				callType: enforcement.ModeBlock,
			}
			resolved := enforcement.ResolveMode(enforcement.ModeAllow, overrides, callType)
			assert.Equal(t, enforcement.ModeBlock, resolved,
				"per-call-type override for %q should take precedence", callType)
		})
	}
}

// TestResolveMode_NoMode verifies that when neither a global mode nor
// per-call-type override is configured, the empty mode is returned to
// maintain backward compatibility with legacy behavior.
func TestResolveMode_NoMode(t *testing.T) {
	resolved := enforcement.ResolveMode("", nil, "track")
	assert.Equal(t, enforcement.Mode(""), resolved)
}

// TestResolveMode_EmptyEventType verifies that when the event type is empty,
// the global mode is returned directly (no per-call-type lookup is possible).
func TestResolveMode_EmptyEventType(t *testing.T) {
	overrides := map[string]enforcement.Mode{
		"track": enforcement.ModeOmit,
	}
	resolved := enforcement.ResolveMode(enforcement.ModeBlock, overrides, "")
	assert.Equal(t, enforcement.ModeBlock, resolved)
}

// TestResolveMode_InvalidPerCallTypeMode verifies that when a per-call-type
// override contains an invalid mode value, the system falls back to the global
// mode. The implementation validates overrides via IsValidMode and ignores
// invalid ones to prevent misconfiguration from affecting pipeline behavior.
func TestResolveMode_InvalidPerCallTypeMode(t *testing.T) {
	overrides := map[string]enforcement.Mode{
		"track": "invalid",
	}
	resolved := enforcement.ResolveMode(enforcement.ModeAllow, overrides, "track")
	assert.Equal(t, enforcement.ModeAllow, resolved,
		"invalid per-call-type mode should fall back to global")
}

// ---------------------------------------------------------------------------
// Phase 4: Test MergedTpConfig Integration (ResolveModeFromConfig)
// ---------------------------------------------------------------------------

// TestResolveModeFromConfig_WithEnforcementMode verifies that the global
// enforcement mode is correctly extracted from the MergedTpConfig map.
func TestResolveModeFromConfig_WithEnforcementMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		expected enforcement.Mode
	}{
		{"block", "block", enforcement.ModeBlock},
		{"omit", "omit", enforcement.ModeOmit},
		{"allow", "allow", enforcement.ModeAllow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := map[string]any{"enforcementMode": tc.mode}
			resolved := enforcement.ResolveModeFromConfig(config, "track")
			assert.Equal(t, tc.expected, resolved)
		})
	}
}

// TestResolveModeFromConfig_WithPerCallTypeOverride verifies that per-call-type
// overrides stored as "enforcementMode_<eventType>" keys in MergedTpConfig
// take precedence over the global enforcementMode.
func TestResolveModeFromConfig_WithPerCallTypeOverride(t *testing.T) {
	config := map[string]any{
		"enforcementMode":       "allow",
		"enforcementMode_track": "block",
	}

	// Per-call-type override for track
	resolved := enforcement.ResolveModeFromConfig(config, "track")
	assert.Equal(t, enforcement.ModeBlock, resolved)

	// No override for identify — falls back to global
	resolved = enforcement.ResolveModeFromConfig(config, "identify")
	assert.Equal(t, enforcement.ModeAllow, resolved)
}

// TestResolveModeFromConfig_WithPropagateValidationErrors verifies backward
// compatibility: when only the legacy propagateValidationErrors toggle is
// present (no enforcementMode), ResolveModeFromConfig returns empty Mode so
// the processor can fall back to the legacy toggle behavior.
func TestResolveModeFromConfig_WithPropagateValidationErrors(t *testing.T) {
	config := map[string]any{
		"propagateValidationErrors": "true",
	}
	resolved := enforcement.ResolveModeFromConfig(config, "track")
	assert.Equal(t, enforcement.Mode(""), resolved,
		"legacy propagateValidationErrors should not be converted to a mode")
}

// TestResolveModeFromConfig_EmptyConfig verifies correct behavior when the
// MergedTpConfig is empty or nil — no enforcement should be configured.
func TestResolveModeFromConfig_EmptyConfig(t *testing.T) {
	// Empty map
	resolved := enforcement.ResolveModeFromConfig(map[string]any{}, "track")
	assert.Equal(t, enforcement.Mode(""), resolved)

	// Nil map — must not panic
	require.NotPanics(t, func() {
		resolved = enforcement.ResolveModeFromConfig(nil, "track")
	})
	assert.Equal(t, enforcement.Mode(""), resolved)
}

// TestResolveModeFromConfig_InvalidGlobalMode verifies that an invalid
// global enforcement mode string is passed through as-is (the implementation
// returns it from ResolveMode as the global fallback without re-validating).
func TestResolveModeFromConfig_InvalidGlobalMode(t *testing.T) {
	config := map[string]any{"enforcementMode": "INVALID"}
	resolved := enforcement.ResolveModeFromConfig(config, "track")
	// The implementation converts to Mode("INVALID") and passes it to ResolveMode
	// as globalMode; since no valid per-call-type override exists, globalMode is returned.
	assert.Equal(t, enforcement.Mode("INVALID"), resolved)
}

// TestResolveModeFromConfig_InvalidPerCallTypeOverride verifies that an
// invalid per-call-type override is ignored, falling back to the global mode.
func TestResolveModeFromConfig_InvalidPerCallTypeOverride(t *testing.T) {
	config := map[string]any{
		"enforcementMode":       "allow",
		"enforcementMode_track": "GARBAGE",
	}
	// "GARBAGE" fails IsValidMode, so override is not applied
	resolved := enforcement.ResolveModeFromConfig(config, "track")
	assert.Equal(t, enforcement.ModeAllow, resolved)
}

// TestResolveModeFromConfig_NonStringValues verifies that non-string values
// in the config are handled gracefully without panics.
func TestResolveModeFromConfig_NonStringValues(t *testing.T) {
	t.Run("integer enforcementMode", func(t *testing.T) {
		config := map[string]any{"enforcementMode": 42}
		require.NotPanics(t, func() {
			resolved := enforcement.ResolveModeFromConfig(config, "track")
			assert.Equal(t, enforcement.Mode(""), resolved)
		})
	})
	t.Run("bool enforcementMode", func(t *testing.T) {
		config := map[string]any{"enforcementMode": true}
		require.NotPanics(t, func() {
			resolved := enforcement.ResolveModeFromConfig(config, "track")
			assert.Equal(t, enforcement.Mode(""), resolved)
		})
	})
	t.Run("non-string override", func(t *testing.T) {
		config := map[string]any{
			"enforcementMode":       "allow",
			"enforcementMode_track": 123,
		}
		require.NotPanics(t, func() {
			resolved := enforcement.ResolveModeFromConfig(config, "track")
			assert.Equal(t, enforcement.ModeAllow, resolved)
		})
	})
}

// TestResolveModeFromConfig_AllCallTypesOverrides verifies that per-call-type
// config keys work for all five supported event types.
func TestResolveModeFromConfig_AllCallTypesOverrides(t *testing.T) {
	for _, callType := range enforcement.SupportedCallTypes {
		t.Run(callType, func(t *testing.T) {
			config := map[string]any{
				"enforcementMode":             "allow",
				"enforcementMode_" + callType: "block",
			}
			resolved := enforcement.ResolveModeFromConfig(config, callType)
			assert.Equal(t, enforcement.ModeBlock, resolved,
				"per-call-type override for %q should apply", callType)
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 5: Test Mode Behavioral Properties
// ---------------------------------------------------------------------------

// TestModeBlock_ShouldRejectEvent verifies that Block mode rejects the event.
func TestModeBlock_ShouldRejectEvent(t *testing.T) {
	assert.True(t, enforcement.ShouldRejectEvent(enforcement.ModeBlock))
}

// TestModeOmit_ShouldNotRejectEvent verifies that Omit mode does NOT reject.
func TestModeOmit_ShouldNotRejectEvent(t *testing.T) {
	assert.False(t, enforcement.ShouldRejectEvent(enforcement.ModeOmit))
}

// TestModeAllow_ShouldNotRejectEvent verifies that Allow mode does NOT reject.
func TestModeAllow_ShouldNotRejectEvent(t *testing.T) {
	assert.False(t, enforcement.ShouldRejectEvent(enforcement.ModeAllow))
}

// TestModeBlock_ShouldStripProperties verifies that Block mode does NOT strip
// properties — the entire event is rejected instead.
func TestModeBlock_ShouldStripProperties(t *testing.T) {
	assert.False(t, enforcement.ShouldStripProperties(enforcement.ModeBlock))
}

// TestModeOmit_ShouldStripProperties verifies that Omit mode strips
// non-conforming properties from the event.
func TestModeOmit_ShouldStripProperties(t *testing.T) {
	assert.True(t, enforcement.ShouldStripProperties(enforcement.ModeOmit))
}

// TestModeAllow_ShouldStripProperties verifies that Allow mode does NOT strip.
func TestModeAllow_ShouldStripProperties(t *testing.T) {
	assert.False(t, enforcement.ShouldStripProperties(enforcement.ModeAllow))
}

// TestModeBlock_ShouldLogViolation verifies that Block mode logs violations.
func TestModeBlock_ShouldLogViolation(t *testing.T) {
	assert.True(t, enforcement.ShouldLogViolation(enforcement.ModeBlock))
}

// TestModeOmit_ShouldLogViolation verifies that Omit mode logs violations.
func TestModeOmit_ShouldLogViolation(t *testing.T) {
	assert.True(t, enforcement.ShouldLogViolation(enforcement.ModeOmit))
}

// TestModeAllow_ShouldLogViolation verifies that Allow mode logs violations.
func TestModeAllow_ShouldLogViolation(t *testing.T) {
	assert.True(t, enforcement.ShouldLogViolation(enforcement.ModeAllow))
}

// TestEmptyMode_DefaultBehavior verifies that the empty mode (no enforcement)
// defaults to legacy behavior: no rejection, no stripping, and no violation
// logging through the enforcement system.
func TestEmptyMode_DefaultBehavior(t *testing.T) {
	empty := enforcement.Mode("")
	assert.False(t, enforcement.ShouldRejectEvent(empty),
		"empty mode should not reject events")
	assert.False(t, enforcement.ShouldStripProperties(empty),
		"empty mode should not strip properties")
	assert.False(t, enforcement.ShouldLogViolation(empty),
		"empty mode should not log violations via enforcement system")
}

// TestBehavioralProperties_TableDriven provides a comprehensive table-driven
// test of all behavioral helper functions across all modes and the empty mode.
func TestBehavioralProperties_TableDriven(t *testing.T) {
	tests := []struct {
		name         string
		mode         enforcement.Mode
		shouldReject bool
		shouldStrip  bool
		shouldLog    bool
	}{
		{"ModeBlock", enforcement.ModeBlock, true, false, true},
		{"ModeOmit", enforcement.ModeOmit, false, true, true},
		{"ModeAllow", enforcement.ModeAllow, false, false, true},
		{"empty mode", "", false, false, false},
		{"invalid mode", "invalid", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.shouldReject, enforcement.ShouldRejectEvent(tc.mode),
				"ShouldRejectEvent mismatch for %q", tc.mode)
			assert.Equal(t, tc.shouldStrip, enforcement.ShouldStripProperties(tc.mode),
				"ShouldStripProperties mismatch for %q", tc.mode)
			assert.Equal(t, tc.shouldLog, enforcement.ShouldLogViolation(tc.mode),
				"ShouldLogViolation mismatch for %q", tc.mode)
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 6: Test Table-Driven Mode Combinations (ResolveMode)
// ---------------------------------------------------------------------------

// TestResolveMode_TableDriven exercises ResolveMode with a comprehensive
// matrix of global modes, per-call-type overrides, and event types.
func TestResolveMode_TableDriven(t *testing.T) {
	tests := []struct {
		name         string
		globalMode   enforcement.Mode
		perCallType  map[string]enforcement.Mode
		eventType    string
		expectedMode enforcement.Mode
	}{
		{
			name:         "global block only",
			globalMode:   enforcement.ModeBlock,
			perCallType:  nil,
			eventType:    "track",
			expectedMode: enforcement.ModeBlock,
		},
		{
			name:         "global omit only",
			globalMode:   enforcement.ModeOmit,
			perCallType:  nil,
			eventType:    "identify",
			expectedMode: enforcement.ModeOmit,
		},
		{
			name:         "global allow only",
			globalMode:   enforcement.ModeAllow,
			perCallType:  nil,
			eventType:    "page",
			expectedMode: enforcement.ModeAllow,
		},
		{
			name:       "override track to block",
			globalMode: enforcement.ModeAllow,
			perCallType: map[string]enforcement.Mode{
				"track": enforcement.ModeBlock,
			},
			eventType:    "track",
			expectedMode: enforcement.ModeBlock,
		},
		{
			name:       "no override for identify",
			globalMode: enforcement.ModeAllow,
			perCallType: map[string]enforcement.Mode{
				"track": enforcement.ModeBlock,
			},
			eventType:    "identify",
			expectedMode: enforcement.ModeAllow,
		},
		{
			name:       "all types overridden — identify uses omit",
			globalMode: enforcement.ModeAllow,
			perCallType: map[string]enforcement.Mode{
				"track":    enforcement.ModeBlock,
				"identify": enforcement.ModeOmit,
			},
			eventType:    "identify",
			expectedMode: enforcement.ModeOmit,
		},
		{
			name:         "empty global no override",
			globalMode:   "",
			perCallType:  nil,
			eventType:    "track",
			expectedMode: "",
		},
		{
			name:         "empty event type with global",
			globalMode:   enforcement.ModeBlock,
			perCallType:  nil,
			eventType:    "",
			expectedMode: enforcement.ModeBlock,
		},
		{
			name:       "empty event type ignores overrides",
			globalMode: enforcement.ModeAllow,
			perCallType: map[string]enforcement.Mode{
				"track": enforcement.ModeBlock,
			},
			eventType:    "",
			expectedMode: enforcement.ModeAllow,
		},
		{
			name:       "invalid override falls back to global",
			globalMode: enforcement.ModeOmit,
			perCallType: map[string]enforcement.Mode{
				"track": "BAD",
			},
			eventType:    "track",
			expectedMode: enforcement.ModeOmit,
		},
		{
			name:         "empty map override falls back to global",
			globalMode:   enforcement.ModeBlock,
			perCallType:  map[string]enforcement.Mode{},
			eventType:    "track",
			expectedMode: enforcement.ModeBlock,
		},
		{
			name:       "screen event type with override",
			globalMode: enforcement.ModeAllow,
			perCallType: map[string]enforcement.Mode{
				"screen": enforcement.ModeOmit,
			},
			eventType:    "screen",
			expectedMode: enforcement.ModeOmit,
		},
		{
			name:       "group event type with override",
			globalMode: enforcement.ModeBlock,
			perCallType: map[string]enforcement.Mode{
				"group": enforcement.ModeAllow,
			},
			eventType:    "group",
			expectedMode: enforcement.ModeAllow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolved := enforcement.ResolveMode(tc.globalMode, tc.perCallType, tc.eventType)
			require.Equal(t, tc.expectedMode, resolved)
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 7: Edge Cases
// ---------------------------------------------------------------------------

// TestMode_TypeCasting verifies that the Mode type is a string alias and
// explicit casting produces the expected constant value.
func TestMode_TypeCasting(t *testing.T) {
	assert.Equal(t, enforcement.ModeBlock, enforcement.Mode("block"))
	assert.Equal(t, enforcement.ModeOmit, enforcement.Mode("omit"))
	assert.Equal(t, enforcement.ModeAllow, enforcement.Mode("allow"))
}

// TestMode_ZeroValue verifies that the zero value of Mode is the empty string,
// which represents no enforcement (backward-compatible legacy behavior).
func TestMode_ZeroValue(t *testing.T) {
	var m enforcement.Mode
	assert.Equal(t, enforcement.Mode(""), m)
	assert.Equal(t, "", string(m))
}

// TestAllModes_ContainsExpectedModes verifies that the AllModes slice contains
// exactly the three valid enforcement modes in the expected order.
func TestAllModes_ContainsExpectedModes(t *testing.T) {
	require.Equal(t, 3, len(enforcement.AllModes))
	assert.Contains(t, enforcement.AllModes, enforcement.ModeBlock)
	assert.Contains(t, enforcement.AllModes, enforcement.ModeOmit)
	assert.Contains(t, enforcement.AllModes, enforcement.ModeAllow)
}

// TestSupportedCallTypes_ContainsExpectedTypes verifies that the
// SupportedCallTypes slice contains all five RudderStack event types.
func TestSupportedCallTypes_ContainsExpectedTypes(t *testing.T) {
	expected := []string{"track", "identify", "group", "page", "screen"}
	require.Equal(t, len(expected), len(enforcement.SupportedCallTypes))
	for _, ct := range expected {
		assert.Contains(t, enforcement.SupportedCallTypes, ct)
	}
}

// TestGetForwardSourceID_Configured verifies that GetForwardSourceID correctly
// extracts the forward-blocked-events source ID from MergedTpConfig.
func TestGetForwardSourceID_Configured(t *testing.T) {
	config := map[string]any{
		"forwardBlockedEventsSourceId": "alternative-source-123",
	}
	assert.Equal(t, "alternative-source-123", enforcement.GetForwardSourceID(config))
}

// TestGetForwardSourceID_NotConfigured verifies that GetForwardSourceID
// returns an empty string when the key is absent from the config.
func TestGetForwardSourceID_NotConfigured(t *testing.T) {
	assert.Equal(t, "", enforcement.GetForwardSourceID(map[string]any{}))
}

// TestGetForwardSourceID_NilConfig verifies that GetForwardSourceID handles
// nil config gracefully without panicking.
func TestGetForwardSourceID_NilConfig(t *testing.T) {
	require.NotPanics(t, func() {
		result := enforcement.GetForwardSourceID(nil)
		assert.Equal(t, "", result)
	})
}

// TestGetForwardSourceID_NonStringValue verifies that GetForwardSourceID
// returns an empty string when the config key holds a non-string value.
func TestGetForwardSourceID_NonStringValue(t *testing.T) {
	config := map[string]any{
		"forwardBlockedEventsSourceId": 12345,
	}
	assert.Equal(t, "", enforcement.GetForwardSourceID(config))
}

// TestGetForwardSourceID_EmptyString verifies that an explicitly empty
// forward source ID is returned as-is (empty string).
func TestGetForwardSourceID_EmptyString(t *testing.T) {
	config := map[string]any{
		"forwardBlockedEventsSourceId": "",
	}
	assert.Equal(t, "", enforcement.GetForwardSourceID(config))
}

// TestResolveModeFromConfig_EmptyEventType verifies behavior when the
// event type is empty — should still return the global enforcement mode.
func TestResolveModeFromConfig_EmptyEventType(t *testing.T) {
	config := map[string]any{
		"enforcementMode":       "omit",
		"enforcementMode_track": "block",
	}
	resolved := enforcement.ResolveModeFromConfig(config, "")
	assert.Equal(t, enforcement.ModeOmit, resolved,
		"empty event type should use global mode")
}

// TestIsValidMode_AllModesSlice verifies that every mode in the AllModes
// slice passes the IsValidMode check.
func TestIsValidMode_AllModesSlice(t *testing.T) {
	for _, m := range enforcement.AllModes {
		assert.True(t, enforcement.IsValidMode(m),
			"mode %q from AllModes should be valid", m)
	}
}
