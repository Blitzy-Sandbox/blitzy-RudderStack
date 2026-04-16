// Package graph — externalids_test.go contains comprehensive unit tests for the
// external ID management module (E-028). Tests cover the 12+ default external
// identifier types, context.externalId event processing/extraction, identifier
// type validation, deduplication, filtering with settings integration, priority
// sorting, and edge cases.
package graph

import (
	"testing"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-server/identity/settings"
)

// ---------------------------------------------------------------------------
// Phase 1: External ID Type Constants Tests
// ---------------------------------------------------------------------------

// TestDefaultExternalIDTypes verifies that the DefaultExternalIDTypes variable
// contains at least 12 entries matching the Segment Unify specification
// (refs/segment-docs/src/unify/identity-resolution/externalids.md), has no
// duplicates, and includes all required identifier types.
func TestDefaultExternalIDTypes(t *testing.T) {
	// Verify at least 12 entries as required by E-028
	require.GreaterOrEqual(t, len(DefaultExternalIDTypes), 12,
		"DefaultExternalIDTypes must contain at least 12 entries per Segment Unify specification")

	// Verify no duplicates in the list
	seen := make(map[string]struct{}, len(DefaultExternalIDTypes))
	for _, idType := range DefaultExternalIDTypes {
		_, exists := seen[idType]
		require.False(t, exists, "duplicate found in DefaultExternalIDTypes: %s", idType)
		seen[idType] = struct{}{}
	}

	// Table-driven test: verify ALL required Segment default types exist.
	// Sourced from refs/segment-docs/src/unify/identity-resolution/externalids.md
	requiredTypes := []struct {
		name   string
		idType string
	}{
		{name: "user_id", idType: "user_id"},
		{name: "email", idType: "email"},
		{name: "anonymous_id", idType: "anonymous_id"},
		{name: "ios.id", idType: "ios.id"},
		{name: "ios.idfa", idType: "ios.idfa"},
		{name: "ios.push_token", idType: "ios.push_token"},
		{name: "android.id", idType: "android.id"},
		{name: "android.push_token", idType: "android.push_token"},
		{name: "ga_client_id", idType: "ga_client_id"},
		{name: "cross_domain_id", idType: "cross_domain_id"},
		{name: "braze_id", idType: "braze_id"},
	}
	for _, tc := range requiredTypes {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, DefaultExternalIDTypes, tc.idType,
				"DefaultExternalIDTypes must contain %q", tc.idType)
		})
	}

	// Verify android.aaid or android.idfa is present (Segment docs list
	// android.idfa/android.aaid as the advertising ID on Android).
	t.Run("android.aaid_or_android.idfa", func(t *testing.T) {
		hasAAID := false
		hasIDFA := false
		for _, idType := range DefaultExternalIDTypes {
			if idType == "android.aaid" {
				hasAAID = true
			}
			if idType == "android.idfa" {
				hasIDFA = true
			}
		}
		require.True(t, hasAAID || hasIDFA,
			"DefaultExternalIDTypes must contain either android.aaid or android.idfa")
	})
}

// TestIsKnownExternalIDType verifies the IsKnownExternalIDType function returns
// true for all default types and false for unknown/empty types.
func TestIsKnownExternalIDType(t *testing.T) {
	tests := []struct {
		name     string
		idType   string
		expected bool
	}{
		{name: "user_id is known", idType: "user_id", expected: true},
		{name: "email is known", idType: "email", expected: true},
		{name: "anonymous_id is known", idType: "anonymous_id", expected: true},
		{name: "ios.idfa is known", idType: "ios.idfa", expected: true},
		{name: "braze_id is known", idType: "braze_id", expected: true},
		{name: "ga_client_id is known", idType: "ga_client_id", expected: true},
		{name: "custom_type is not known", idType: "custom_type", expected: false},
		{name: "empty string is not known", idType: "", expected: false},
		{name: "unknown_random is not known", idType: "unknown_random", expected: false},
		{name: "case sensitive check", idType: "User_Id", expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsKnownExternalIDType(tt.idType)
			if tt.expected {
				require.True(t, result, "IsKnownExternalIDType(%q) should return true", tt.idType)
			} else {
				require.False(t, result, "IsKnownExternalIDType(%q) should return false", tt.idType)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 2: Event Parsing Tests — Extracting External IDs from Events
// ---------------------------------------------------------------------------

// TestExtractExternalIDs_FromContextExternalIds verifies extraction from the
// context.externalId array, following the pattern from
// salesforce_bulk.go:84 — gjson.GetBytes(job.EventPayload, "context.externalId").
func TestExtractExternalIDs_FromContextExternalIds(t *testing.T) {
	eventJSON := []byte(`{
		"context": {
			"externalId": [
				{"type": "braze_id", "id": "braze-123"},
				{"type": "ga_client_id", "id": "GA1.2.345"}
			]
		}
	}`)

	ids := ExtractExternalIDs(eventJSON)
	require.NotNil(t, ids)
	require.Len(t, ids, 2, "should extract exactly 2 external IDs from context.externalId")

	// Build lookup map for assertion convenience
	found := identifierMap(ids)
	require.Equal(t, "braze-123", found["braze_id"], "braze_id value must be extracted")
	require.Equal(t, "GA1.2.345", found["ga_client_id"], "ga_client_id value must be extracted")
}

// TestExtractExternalIDs_FromUserIDAndAnonymousID verifies extraction of
// top-level userId and anonymousId fields from standard RudderStack events.
func TestExtractExternalIDs_FromUserIDAndAnonymousID(t *testing.T) {
	eventJSON := []byte(`{
		"userId": "user-123",
		"anonymousId": "anon-uuid-456"
	}`)

	ids := ExtractExternalIDs(eventJSON)
	require.NotNil(t, ids)
	require.Len(t, ids, 2)

	found := identifierMap(ids)
	require.Equal(t, "user-123", found["user_id"], "userId should map to user_id identifier")
	require.Equal(t, "anon-uuid-456", found["anonymous_id"], "anonymousId should map to anonymous_id identifier")
}

// TestExtractExternalIDs_FromTraits verifies that traits.email is extracted
// from identify events containing identity-relevant traits.
func TestExtractExternalIDs_FromTraits(t *testing.T) {
	eventJSON := []byte(`{
		"userId": "user-123",
		"traits": {
			"email": "user@example.com"
		}
	}`)

	ids := ExtractExternalIDs(eventJSON)
	require.NotNil(t, ids)

	found := identifierMap(ids)
	require.Equal(t, "user@example.com", found["email"], "traits.email should be extracted as email identifier")
	require.Equal(t, "user-123", found["user_id"], "userId should also be extracted")
}

// TestExtractExternalIDs_FromContextTraitsEmail verifies fallback extraction
// from context.traits.email when traits.email is not present.
func TestExtractExternalIDs_FromContextTraitsEmail(t *testing.T) {
	eventJSON := []byte(`{
		"userId": "user-123",
		"context": {
			"traits": {
				"email": "ctx-user@example.com"
			}
		}
	}`)

	ids := ExtractExternalIDs(eventJSON)
	require.NotNil(t, ids)

	found := identifierMap(ids)
	require.Equal(t, "ctx-user@example.com", found["email"],
		"context.traits.email should be extracted as fallback when traits.email is absent")
}

// TestExtractExternalIDs_Combined verifies extraction from all four sources
// simultaneously: userId, anonymousId, traits.email, and context.externalId.
func TestExtractExternalIDs_Combined(t *testing.T) {
	eventJSON := []byte(`{
		"userId": "user-123",
		"anonymousId": "anon-uuid",
		"traits": {"email": "user@example.com"},
		"context": {
			"externalId": [
				{"type": "braze_id", "id": "braze-123"},
				{"type": "ios.idfa", "id": "idfa-456"}
			]
		}
	}`)

	ids := ExtractExternalIDs(eventJSON)
	require.NotNil(t, ids)
	require.Len(t, ids, 5, "should extract 5 identifiers: user_id, anonymous_id, email, braze_id, ios.idfa")

	// Verify all expected types are present
	typeSet := make(map[string]struct{})
	for _, id := range ids {
		typeSet[id.Type] = struct{}{}
	}
	for _, expectedType := range []string{"user_id", "anonymous_id", "email", "braze_id", "ios.idfa"} {
		_, exists := typeSet[expectedType]
		require.True(t, exists, "expected identifier type %q not found in result", expectedType)
	}

	// Verify no duplicates (even if same value appears in multiple sources)
	seen := make(map[string]struct{})
	for _, id := range ids {
		key := id.Key()
		_, exists := seen[key]
		require.False(t, exists, "duplicate identifier found: %s", key)
		seen[key] = struct{}{}
	}
}

// TestExtractExternalIDs_EmptyEvent verifies that an empty JSON object returns
// an empty/nil slice and does not panic.
func TestExtractExternalIDs_EmptyEvent(t *testing.T) {
	ids := ExtractExternalIDs([]byte(`{}`))
	require.Empty(t, ids, "empty event should produce empty identifiers")
}

// TestExtractExternalIDs_NilInput verifies that nil input returns nil.
func TestExtractExternalIDs_NilInput(t *testing.T) {
	ids := ExtractExternalIDs(nil)
	require.Nil(t, ids, "nil input should return nil")
}

// TestExtractExternalIDs_EmptyInput verifies that empty byte slice returns nil.
func TestExtractExternalIDs_EmptyInput(t *testing.T) {
	ids := ExtractExternalIDs([]byte{})
	require.Nil(t, ids, "empty byte slice should return nil")
}

// TestExtractExternalIDs_NullUserId verifies that a null userId is skipped.
func TestExtractExternalIDs_NullUserId(t *testing.T) {
	eventJSON := []byte(`{"userId": null}`)
	ids := ExtractExternalIDs(eventJSON)
	require.Empty(t, ids, "null userId should be skipped — no identifiers extracted")
}

// TestExtractExternalIDs_EmptyStringUserId verifies that an empty string userId
// is skipped and not added as an identifier.
func TestExtractExternalIDs_EmptyStringUserId(t *testing.T) {
	eventJSON := []byte(`{"userId": ""}`)
	ids := ExtractExternalIDs(eventJSON)
	require.Empty(t, ids, "empty string userId should be skipped")
}

// TestExtractExternalIDs_WhitespaceOnlyUserId verifies that whitespace-only userId
// is trimmed and treated as empty.
func TestExtractExternalIDs_WhitespaceOnlyUserId(t *testing.T) {
	eventJSON := []byte(`{"userId": "   "}`)
	ids := ExtractExternalIDs(eventJSON)
	require.Empty(t, ids, "whitespace-only userId should be skipped after trimming")
}

// TestExtractExternalIDs_MalformedExternalIdArray verifies graceful handling
// when context.externalId contains non-object entries.
func TestExtractExternalIDs_MalformedExternalIdArray(t *testing.T) {
	eventJSON := []byte(`{
		"userId": "user-123",
		"context": {
			"externalId": [
				{"type": "braze_id", "id": "braze-123"},
				"invalid_string_entry",
				{"type": "", "id": "no-type"},
				{"type": "ga_client_id", "id": ""}
			]
		}
	}`)

	ids := ExtractExternalIDs(eventJSON)
	require.NotNil(t, ids)

	// user_id from top-level + braze_id from valid entry = 2 valid identifiers
	// Empty type/value entries should be filtered out
	found := identifierMap(ids)
	require.Equal(t, "user-123", found["user_id"])
	require.Equal(t, "braze-123", found["braze_id"])

	// Ensure entries with empty type or value are excluded
	for _, id := range ids {
		require.NotEmpty(t, id.Type, "no identifier with empty type should be present")
		require.NotEmpty(t, id.Value, "no identifier with empty value should be present")
	}
}

// ---------------------------------------------------------------------------
// Phase 3: Identifier Pair Type Tests
// ---------------------------------------------------------------------------

// TestIdentifierPair_Struct verifies that the IdentifierPair struct has the
// expected fields and JSON serialization format.
func TestIdentifierPair_Struct(t *testing.T) {
	pair := IdentifierPair{Type: "user_id", Value: "user-123"}

	// Verify struct field access
	require.Equal(t, "user_id", pair.Type)
	require.Equal(t, "user-123", pair.Value)

	// Verify JSON serialization produces expected format with json struct tags
	data, err := jsonrs.Marshal(pair)
	require.Nil(t, err)
	require.NotNil(t, data)

	var decoded map[string]string
	err = jsonrs.Unmarshal(data, &decoded)
	require.Nil(t, err)
	require.Equal(t, "user_id", decoded["type"], "JSON key should be 'type' per json tag")
	require.Equal(t, "user-123", decoded["value"], "JSON key should be 'value' per json tag")
}

// TestIdentifierPair_Equality verifies Go struct equality semantics for IdentifierPair.
func TestIdentifierPair_Equality(t *testing.T) {
	t.Run("same Type and Value are equal", func(t *testing.T) {
		a := IdentifierPair{Type: "user_id", Value: "u1"}
		b := IdentifierPair{Type: "user_id", Value: "u1"}
		require.Equal(t, a, b, "identical IdentifierPairs should be equal")
	})

	t.Run("different Type not equal", func(t *testing.T) {
		a := IdentifierPair{Type: "user_id", Value: "u1"}
		b := IdentifierPair{Type: "email", Value: "u1"}
		require.NotEqual(t, a, b, "pairs with different Type should not be equal")
	})

	t.Run("different Value not equal", func(t *testing.T) {
		a := IdentifierPair{Type: "user_id", Value: "u1"}
		b := IdentifierPair{Type: "user_id", Value: "u2"}
		require.NotEqual(t, a, b, "pairs with different Value should not be equal")
	})
}

// TestIdentifierPair_IsEmpty verifies the IsEmpty method for various combinations
// of Type and Value fields.
func TestIdentifierPair_IsEmpty(t *testing.T) {
	tests := []struct {
		name     string
		pair     IdentifierPair
		expected bool
	}{
		{
			name:     "both empty strings",
			pair:     IdentifierPair{Type: "", Value: ""},
			expected: true,
		},
		{
			name:     "value empty — type present",
			pair:     IdentifierPair{Type: "user_id", Value: ""},
			expected: true,
		},
		{
			name:     "type empty — value present",
			pair:     IdentifierPair{Type: "", Value: "abc"},
			expected: true,
		},
		{
			name:     "neither empty — valid pair",
			pair:     IdentifierPair{Type: "user_id", Value: "abc"},
			expected: false,
		},
		{
			name:     "whitespace only type",
			pair:     IdentifierPair{Type: "  ", Value: "abc"},
			expected: true,
		},
		{
			name:     "whitespace only value",
			pair:     IdentifierPair{Type: "user_id", Value: "   "},
			expected: true,
		},
		{
			name:     "both whitespace only",
			pair:     IdentifierPair{Type: "  ", Value: "  "},
			expected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.pair.IsEmpty()
			require.Equal(t, tt.expected, result,
				"IsEmpty() for pair {%q, %q} should be %v", tt.pair.Type, tt.pair.Value, tt.expected)
		})
	}
}

// TestIdentifierPair_Key verifies the Key method returns the expected
// "type:value" concatenation for deduplication.
func TestIdentifierPair_Key(t *testing.T) {
	pair := IdentifierPair{Type: "user_id", Value: "user-123"}
	require.Equal(t, "user_id:user-123", pair.Key())

	pair2 := IdentifierPair{Type: "email", Value: "user@example.com"}
	require.Equal(t, "email:user@example.com", pair2.Key())

	// Different keys for different pairs
	require.NotEqual(t, pair.Key(), pair2.Key())
}

// ---------------------------------------------------------------------------
// Phase 4: Deduplication Tests
// ---------------------------------------------------------------------------

// TestDeduplicateIdentifierPairs verifies that deduplicateExtractedIdentifiers
// removes exact duplicates (same Type AND Value) while preserving distinct
// entries and original order.
func TestDeduplicateIdentifierPairs(t *testing.T) {
	input := []IdentifierPair{
		{Type: "user_id", Value: "u1"},
		{Type: "email", Value: "e1"},
		{Type: "user_id", Value: "u1"}, // exact duplicate — should be removed
		{Type: "email", Value: "e2"},   // same type, different value — NOT a duplicate
	}

	result := deduplicateExtractedIdentifiers(input)
	require.Len(t, result, 3, "should remove 1 duplicate, keeping 3 unique pairs")
	require.Equal(t, IdentifierPair{Type: "user_id", Value: "u1"}, result[0])
	require.Equal(t, IdentifierPair{Type: "email", Value: "e1"}, result[1])
	require.Equal(t, IdentifierPair{Type: "email", Value: "e2"}, result[2])
}

// TestDeduplicateIdentifierPairs_Empty verifies that empty and nil inputs
// produce empty results without panicking.
func TestDeduplicateIdentifierPairs_Empty(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		result := deduplicateExtractedIdentifiers(nil)
		require.Empty(t, result)
	})

	t.Run("empty slice", func(t *testing.T) {
		result := deduplicateExtractedIdentifiers([]IdentifierPair{})
		require.Empty(t, result)
	})
}

// TestDeduplicateIdentifierPairs_AllUnique verifies that an input with no
// duplicates returns the same elements in the same order.
func TestDeduplicateIdentifierPairs_AllUnique(t *testing.T) {
	input := []IdentifierPair{
		{Type: "user_id", Value: "u1"},
		{Type: "email", Value: "e1"},
		{Type: "anonymous_id", Value: "a1"},
	}
	result := deduplicateExtractedIdentifiers(input)
	require.Len(t, result, 3)
	require.Equal(t, input, result, "no duplicates means result should match input")
}

// TestDeduplicateIdentifierPairs_AllDuplicates verifies that an input where
// all entries are duplicates returns only one entry.
func TestDeduplicateIdentifierPairs_AllDuplicates(t *testing.T) {
	input := []IdentifierPair{
		{Type: "user_id", Value: "u1"},
		{Type: "user_id", Value: "u1"},
		{Type: "user_id", Value: "u1"},
	}
	result := deduplicateExtractedIdentifiers(input)
	require.Len(t, result, 1, "all duplicates should collapse to one entry")
	require.Equal(t, IdentifierPair{Type: "user_id", Value: "u1"}, result[0])
}

// ---------------------------------------------------------------------------
// Phase 5: External ID Filtering Tests (Settings Integration)
// ---------------------------------------------------------------------------

// TestFilterBlockedExternalIDs verifies that FilterBlockedIdentifiers correctly
// removes identifiers with blocked values using the settings package's exact
// match and regex rules.
func TestFilterBlockedExternalIDs(t *testing.T) {
	// Create settings with blocked values only for user_id (not anonymous_id)
	s := settings.New(nil)
	err := s.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
		BlockedValues: []settings.BlockedValueRule{
			{Type: "exact", Value: "null"},
			{Type: "regex", Value: `^[0-]*$`},
		},
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
		Priority: 1,
	})
	require.Nil(t, err, "SetIdentifierConfig should not error")

	identifiers := []IdentifierPair{
		{Type: "user_id", Value: "null"},         // blocked: exact match "null"
		{Type: "user_id", Value: "valid-user"},   // not blocked
		{Type: "anonymous_id", Value: "000-000"}, // not blocked (no rules for anonymous_id)
	}

	result := FilterBlockedIdentifiers(identifiers, s)

	// {user_id, "null"} should be filtered OUT
	for _, id := range result {
		if id.Type == "user_id" {
			require.NotEqual(t, "null", id.Value, "{user_id, null} should be filtered out by exact match rule")
		}
	}

	// {user_id, "valid-user"} should be KEPT
	foundValid := false
	for _, id := range result {
		if id.Type == "user_id" && id.Value == "valid-user" {
			foundValid = true
		}
	}
	require.True(t, foundValid, "{user_id, valid-user} should be kept — not blocked")

	// {anonymous_id, "000-000"} should be KEPT (no blocked rules for anonymous_id)
	foundAnon := false
	for _, id := range result {
		if id.Type == "anonymous_id" && id.Value == "000-000" {
			foundAnon = true
		}
	}
	require.True(t, foundAnon,
		"{anonymous_id, 000-000} should be kept when no blocked rules configured for anonymous_id")

	require.Len(t, result, 2, "should have 2 identifiers after filtering (1 blocked)")
}

// TestFilterBlockedExternalIDs_RegexBlocking verifies that regex patterns
// correctly block matching values.
func TestFilterBlockedExternalIDs_RegexBlocking(t *testing.T) {
	s := settings.New(nil)
	err := s.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
		BlockedValues: []settings.BlockedValueRule{
			{Type: "regex", Value: `^[0-]*$`}, // blocks strings of only zeroes and dashes
		},
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
		Priority: 1,
	})
	require.Nil(t, err)

	identifiers := []IdentifierPair{
		{Type: "user_id", Value: "000-000"},      // blocked by regex
		{Type: "user_id", Value: "0000"},         // blocked by regex
		{Type: "user_id", Value: "real-user-42"}, // not blocked
	}

	result := FilterBlockedIdentifiers(identifiers, s)
	require.Len(t, result, 1, "only 'real-user-42' should survive regex filtering")
	require.Equal(t, "real-user-42", result[0].Value)
}

// TestFilterBlockedExternalIDs_NoBlockedValues verifies that when settings have
// no blocked values configured, all identifiers pass through unchanged.
func TestFilterBlockedExternalIDs_NoBlockedValues(t *testing.T) {
	// Create settings with no identifier-specific configuration
	s := settings.New(nil)

	identifiers := []IdentifierPair{
		{Type: "user_id", Value: "null"},
		{Type: "user_id", Value: "valid-user"},
		{Type: "anonymous_id", Value: "000-000"},
	}

	result := FilterBlockedIdentifiers(identifiers, s)
	require.Len(t, result, 3, "all identifiers should pass through with no blocked values configured")
	require.Equal(t, identifiers, result)
}

// TestFilterBlockedExternalIDs_NilSettings verifies nil-safety: when settings
// is nil, all identifiers pass through unchanged.
func TestFilterBlockedExternalIDs_NilSettings(t *testing.T) {
	identifiers := []IdentifierPair{
		{Type: "user_id", Value: "null"},
		{Type: "email", Value: "user@example.com"},
	}

	result := FilterBlockedIdentifiers(identifiers, nil)
	require.Len(t, result, 2, "nil settings should pass all identifiers through")
	require.Equal(t, identifiers, result)
}

// ---------------------------------------------------------------------------
// Phase 6: Custom External ID Type Tests
// ---------------------------------------------------------------------------

// TestExtractExternalIDs_CustomType verifies that custom (non-default) external
// ID types in context.externalId are extracted. The system supports any string as
// an external ID type; the 12+ defaults are just well-known types.
func TestExtractExternalIDs_CustomType(t *testing.T) {
	eventJSON := []byte(`{
		"context": {
			"externalId": [
				{"type": "custom_crm_id", "id": "crm-123"}
			]
		}
	}`)

	ids := ExtractExternalIDs(eventJSON)
	require.NotEmpty(t, ids, "custom types must be extracted from context.externalId")

	// Verify the custom type is NOT in the default list
	require.False(t, IsKnownExternalIDType("custom_crm_id"),
		"custom_crm_id should not be in DefaultExternalIDTypes")

	// Verify it was still extracted
	require.Len(t, ids, 1)
	require.Equal(t, "custom_crm_id", ids[0].Type, "custom external ID type should be extracted")
	require.Equal(t, "crm-123", ids[0].Value, "custom external ID value should be extracted")
}

// TestExtractExternalIDs_MultipleCustomTypes verifies extraction of multiple
// custom types alongside standard types.
func TestExtractExternalIDs_MultipleCustomTypes(t *testing.T) {
	eventJSON := []byte(`{
		"userId": "user-123",
		"context": {
			"externalId": [
				{"type": "custom_crm_id", "id": "crm-123"},
				{"type": "salesforce_id", "id": "sf-456"},
				{"type": "braze_id", "id": "braze-789"}
			]
		}
	}`)

	ids := ExtractExternalIDs(eventJSON)
	require.Len(t, ids, 4, "should extract user_id + 3 external IDs")

	// Map for easy lookup
	found := identifierMap(ids)
	require.Equal(t, "user-123", found["user_id"])
	require.Equal(t, "crm-123", found["custom_crm_id"])
	require.Equal(t, "sf-456", found["salesforce_id"])
	require.Equal(t, "braze-789", found["braze_id"])
}

// TestSortByPriority verifies that SortByPriority orders identifier pairs
// according to the priority configured in settings (lower number = higher priority).
func TestSortByPriority(t *testing.T) {
	// Create settings with explicit priorities: user_id=1, email=2, anonymous_id=3
	s := settings.New(nil)
	err := s.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
		Priority: 1,
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
	})
	require.Nil(t, err)
	err = s.SetIdentifierConfig("email", &settings.IdentifierConfig{
		Priority: 2,
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
	})
	require.Nil(t, err)
	err = s.SetIdentifierConfig("anonymous_id", &settings.IdentifierConfig{
		Priority: 3,
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
	})
	require.Nil(t, err)

	// Input in non-priority order
	identifiers := []IdentifierPair{
		{Type: "email", Value: "e1"},
		{Type: "user_id", Value: "u1"},
		{Type: "anonymous_id", Value: "a1"},
	}

	SortByPriority(identifiers, s)

	require.Equal(t, "user_id", identifiers[0].Type, "user_id should be first (priority 1)")
	require.Equal(t, "u1", identifiers[0].Value)
	require.Equal(t, "email", identifiers[1].Type, "email should be second (priority 2)")
	require.Equal(t, "e1", identifiers[1].Value)
	require.Equal(t, "anonymous_id", identifiers[2].Type, "anonymous_id should be third (priority 3)")
	require.Equal(t, "a1", identifiers[2].Value)
}

// TestSortByPriority_NilSettings verifies that SortByPriority is a no-op
// when settings is nil.
func TestSortByPriority_NilSettings(t *testing.T) {
	identifiers := []IdentifierPair{
		{Type: "email", Value: "e1"},
		{Type: "user_id", Value: "u1"},
	}
	original := make([]IdentifierPair, len(identifiers))
	copy(original, identifiers)

	SortByPriority(identifiers, nil)

	require.Equal(t, original, identifiers, "nil settings should not modify order")
}

// TestSortByPriority_StableSort verifies that SortByPriority uses a stable sort,
// maintaining relative order for identifiers with the same priority.
func TestSortByPriority_StableSort(t *testing.T) {
	s := settings.New(nil)
	err := s.SetIdentifierConfig("user_id", &settings.IdentifierConfig{
		Priority: 1,
		Limit:    settings.IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
	})
	require.Nil(t, err)

	// Both email entries are unconfigured so they share the same computed priority
	identifiers := []IdentifierPair{
		{Type: "email", Value: "first@example.com"},
		{Type: "email", Value: "second@example.com"},
		{Type: "user_id", Value: "u1"},
	}

	SortByPriority(identifiers, s)

	// user_id (priority 1) should come first
	require.Equal(t, "user_id", identifiers[0].Type)
	// The two email entries should maintain their original relative order (stable sort)
	require.Equal(t, "first@example.com", identifiers[1].Value, "stable sort should preserve relative order")
	require.Equal(t, "second@example.com", identifiers[2].Value, "stable sort should preserve relative order")
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

// identifierMap converts a slice of IdentifierPairs into a map[type]value
// for convenient assertion lookups. Note: if multiple pairs share the same type,
// the last value wins (appropriate for tests where uniqueness is expected).
func identifierMap(ids []IdentifierPair) map[string]string {
	m := make(map[string]string, len(ids))
	for _, id := range ids {
		m[id.Type] = id.Value
	}
	return m
}
