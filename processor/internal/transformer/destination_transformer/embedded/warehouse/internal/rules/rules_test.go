package rules

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-server/processor/internal/transformer/destination_transformer/embedded/warehouse/internal/response"
	wtypes "github.com/rudderlabs/rudder-server/processor/internal/transformer/destination_transformer/embedded/warehouse/internal/types"
	"github.com/rudderlabs/rudder-server/processor/types"
)

func TestIsRudderReservedColumn(t *testing.T) {
	testCases := []struct {
		name       string
		eventType  string
		columnName string
		expected   bool
	}{
		{name: "track", eventType: "track", columnName: "id", expected: true},
		{name: "page", eventType: "page", columnName: "id", expected: true},
		{name: "screen", eventType: "screen", columnName: "id", expected: true},
		{name: "identify", eventType: "identify", columnName: "id", expected: true},
		{name: "group", eventType: "group", columnName: "id", expected: true},
		{name: "alias", eventType: "alias", columnName: "id", expected: true},
		{name: "extract", eventType: "extract", columnName: "id", expected: true},
		{name: "not reserved event type", eventType: "not reserved", columnName: "id", expected: false},
		{name: "not reserved column name", eventType: "track", columnName: "not reserved", expected: false},

		// DefaultRules columns across event types — anonymous_id is reserved for all 6 standard event types
		{name: "track anonymous_id", eventType: "track", columnName: "anonymous_id", expected: true},
		{name: "identify anonymous_id", eventType: "identify", columnName: "anonymous_id", expected: true},
		{name: "page anonymous_id", eventType: "page", columnName: "anonymous_id", expected: true},
		{name: "screen anonymous_id", eventType: "screen", columnName: "anonymous_id", expected: true},
		{name: "group anonymous_id", eventType: "group", columnName: "anonymous_id", expected: true},
		{name: "alias anonymous_id", eventType: "alias", columnName: "anonymous_id", expected: true},

		// DefaultRules columns — user_id
		{name: "track user_id", eventType: "track", columnName: "user_id", expected: true},
		{name: "identify user_id", eventType: "identify", columnName: "user_id", expected: true},

		// DefaultRules columns — sent_at
		{name: "track sent_at", eventType: "track", columnName: "sent_at", expected: true},
		{name: "identify sent_at", eventType: "identify", columnName: "sent_at", expected: true},

		// DefaultRules columns — timestamp
		{name: "track timestamp", eventType: "track", columnName: "timestamp", expected: true},
		{name: "identify timestamp", eventType: "identify", columnName: "timestamp", expected: true},

		// DefaultRules columns — received_at
		{name: "track received_at", eventType: "track", columnName: "received_at", expected: true},
		{name: "identify received_at", eventType: "identify", columnName: "received_at", expected: true},

		// DefaultRules columns — original_timestamp
		{name: "track original_timestamp", eventType: "track", columnName: "original_timestamp", expected: true},
		{name: "identify original_timestamp", eventType: "identify", columnName: "original_timestamp", expected: true},

		// DefaultRules columns — channel (ES-007: channel field auto-population verification)
		{name: "track channel", eventType: "track", columnName: "channel", expected: true},
		{name: "identify channel", eventType: "identify", columnName: "channel", expected: true},

		// DefaultRules columns — context_ip
		{name: "track context_ip", eventType: "track", columnName: "context_ip", expected: true},
		{name: "identify context_ip", eventType: "identify", columnName: "context_ip", expected: true},

		// DefaultRules columns — context_request_ip
		{name: "track context_request_ip", eventType: "track", columnName: "context_request_ip", expected: true},
		{name: "identify context_request_ip", eventType: "identify", columnName: "context_request_ip", expected: true},

		// DefaultRules columns — context_passed_ip
		{name: "track context_passed_ip", eventType: "track", columnName: "context_passed_ip", expected: true},
		{name: "identify context_passed_ip", eventType: "identify", columnName: "context_passed_ip", expected: true},

		// TrackRules: event_text is reserved only for track
		{name: "track event_text", eventType: "track", columnName: "event_text", expected: true},
		{name: "identify event_text not reserved", eventType: "identify", columnName: "event_text", expected: false},

		// TrackTableRules: record_id is reserved only for track
		{name: "track record_id", eventType: "track", columnName: "record_id", expected: true},
		{name: "identify record_id not reserved", eventType: "identify", columnName: "record_id", expected: false},

		// PageRules: name is reserved only for page and screen
		{name: "page name", eventType: "page", columnName: "name", expected: true},
		{name: "track name not reserved", eventType: "track", columnName: "name", expected: false},

		// ScreenRules: name is reserved for screen
		{name: "screen name", eventType: "screen", columnName: "name", expected: true},

		// AliasRules: previous_id is reserved only for alias
		{name: "alias previous_id", eventType: "alias", columnName: "previous_id", expected: true},
		{name: "track previous_id not reserved", eventType: "track", columnName: "previous_id", expected: false},

		// GroupRules: group_id is reserved only for group
		{name: "group group_id", eventType: "group", columnName: "group_id", expected: true},
		{name: "track group_id not reserved", eventType: "track", columnName: "group_id", expected: false},

		// ExtractRules: received_at and event are reserved for extract
		{name: "extract received_at", eventType: "extract", columnName: "received_at", expected: true},
		{name: "extract event", eventType: "extract", columnName: "event", expected: true},

		// Negative cases for extract — extract uses only ExtractRules (NOT merged with DefaultRules)
		{name: "extract anonymous_id not reserved", eventType: "extract", columnName: "anonymous_id", expected: false},
		{name: "extract user_id not reserved", eventType: "extract", columnName: "user_id", expected: false},
		{name: "extract channel not reserved", eventType: "extract", columnName: "channel", expected: false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, IsRudderReservedColumn(tc.eventType, tc.columnName))
		})
	}
}

// TestDefaultRulesContainAllExpectedColumns verifies every expected key exists
// in the DefaultRules map, ensuring all Segment Spec common fields are represented
// as reserved warehouse columns (ES-003, E-001).
func TestDefaultRulesContainAllExpectedColumns(t *testing.T) {
	expectedColumns := []string{
		"id",
		"anonymous_id",
		"user_id",
		"sent_at",
		"timestamp",
		"received_at",
		"original_timestamp",
		"channel",
		"context_ip",
		"context_request_ip",
		"context_passed_ip",
	}
	for _, col := range expectedColumns {
		t.Run(col, func(t *testing.T) {
			require.Contains(t, DefaultRules, col)
		})
	}
}

// TestEventTypeSpecificRulesComplete validates that each event-type-specific rule map
// contains the expected reserved columns per the Segment Spec definitions (ES-003, E-001).
func TestEventTypeSpecificRulesComplete(t *testing.T) {
	testCases := []struct {
		name            string
		rules           map[string]Rules
		expectedColumns []string
	}{
		{
			name:            "TrackRules",
			rules:           TrackRules,
			expectedColumns: []string{"event_text"},
		},
		{
			name:            "IdentifyRules",
			rules:           IdentifyRules,
			expectedColumns: []string{"context_ip", "context_request_ip", "context_passed_ip"},
		},
		{
			name:            "PageRules",
			rules:           PageRules,
			expectedColumns: []string{"name"},
		},
		{
			name:            "ScreenRules",
			rules:           ScreenRules,
			expectedColumns: []string{"name"},
		},
		{
			name:            "AliasRules",
			rules:           AliasRules,
			expectedColumns: []string{"previous_id"},
		},
		{
			name:            "GroupRules",
			rules:           GroupRules,
			expectedColumns: []string{"group_id"},
		},
		{
			name:            "ExtractRules",
			rules:           ExtractRules,
			expectedColumns: []string{"id", "received_at", "event"},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, col := range tc.expectedColumns {
				require.Contains(t, tc.rules, col, "rule map %s should contain column %s", tc.name, col)
			}
		})
	}
}

func TestExtractRecordID(t *testing.T) {
	testCases := []struct {
		name             string
		metadata         wtypes.Metadata
		expectedRecordID any
		expectedError    error
	}{
		{name: "recordId is nil", metadata: wtypes.Metadata{RecordID: nil}, expectedRecordID: nil, expectedError: response.ErrRecordIDEmpty},
		{name: "recordId is empty", metadata: wtypes.Metadata{RecordID: ""}, expectedRecordID: nil, expectedError: response.ErrRecordIDEmpty},
		{name: "recordId is not empty", metadata: wtypes.Metadata{RecordID: "123"}, expectedRecordID: "123", expectedError: nil},
		{name: "recordId is an object", metadata: wtypes.Metadata{RecordID: map[string]any{"key": "value"}}, expectedRecordID: nil, expectedError: response.ErrRecordIDObject},
		{name: "recordId is an array", metadata: wtypes.Metadata{RecordID: []any{"value"}}, expectedRecordID: nil, expectedError: response.ErrRecordIDArray},
		{name: "recordId is a string", metadata: wtypes.Metadata{RecordID: "123"}, expectedRecordID: "123", expectedError: nil},
		{name: "recordId is a number", metadata: wtypes.Metadata{RecordID: 123}, expectedRecordID: 123, expectedError: nil},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recordID, err := extractRecordID(&tc.metadata)
			require.Equal(t, tc.expectedError, err)
			require.Equal(t, tc.expectedRecordID, recordID)
		})
	}
}

func TestExtractCloudRecordID(t *testing.T) {
	testCases := []struct {
		name             string
		message          types.SingularEventT
		metadata         wtypes.Metadata
		fallbackValue    any
		expectedRecordID any
		expectedError    error
	}{
		{name: "sources version is nil", message: types.SingularEventT{"context": map[string]any{"sources": map[string]any{"version": nil}}}, metadata: wtypes.Metadata{}, fallbackValue: "fallback", expectedRecordID: "fallback", expectedError: nil},
		{name: "sources version is empty", message: types.SingularEventT{"context": map[string]any{"sources": map[string]any{"version": ""}}}, metadata: wtypes.Metadata{}, fallbackValue: "fallback", expectedRecordID: "fallback", expectedError: nil},
		{name: "sources version is not empty", message: types.SingularEventT{"context": map[string]any{"sources": map[string]any{"version": "1.0"}}}, metadata: wtypes.Metadata{RecordID: "123"}, fallbackValue: "fallback", expectedRecordID: "123", expectedError: nil},
		{name: "recordId is nil", message: types.SingularEventT{"context": map[string]any{"sources": map[string]any{"version": "1.0"}}}, metadata: wtypes.Metadata{}, fallbackValue: "fallback", expectedRecordID: nil, expectedError: response.ErrRecordIDEmpty},
		{name: "recordId is empty", message: types.SingularEventT{"context": map[string]any{"sources": map[string]any{"version": "1.0"}}}, metadata: wtypes.Metadata{RecordID: ""}, fallbackValue: "fallback", expectedRecordID: nil, expectedError: response.ErrRecordIDEmpty},
		{name: "recordId is an object", message: types.SingularEventT{"context": map[string]any{"sources": map[string]any{"version": "1.0"}}}, metadata: wtypes.Metadata{RecordID: map[string]any{"key": "value"}}, fallbackValue: "fallback", expectedRecordID: nil, expectedError: response.ErrRecordIDObject},
		{name: "recordId is an array", message: types.SingularEventT{"context": map[string]any{"sources": map[string]any{"version": "1.0"}}}, metadata: wtypes.Metadata{RecordID: []any{"value"}}, fallbackValue: "fallback", expectedRecordID: nil, expectedError: response.ErrRecordIDArray},
		{name: "recordId is a string", message: types.SingularEventT{"context": map[string]any{"sources": map[string]any{"version": "1.0"}}}, metadata: wtypes.Metadata{RecordID: "123"}, fallbackValue: "fallback", expectedRecordID: "123", expectedError: nil},
		{name: "recordId is a number", message: types.SingularEventT{"context": map[string]any{"sources": map[string]any{"version": "1.0"}}}, metadata: wtypes.Metadata{RecordID: 123}, fallbackValue: "fallback", expectedRecordID: 123, expectedError: nil},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recordID, err := extractCloudRecordID(tc.message, &tc.metadata, tc.fallbackValue)
			require.Equal(t, tc.expectedError, err)
			require.Equal(t, tc.expectedRecordID, recordID)
		})
	}
}
