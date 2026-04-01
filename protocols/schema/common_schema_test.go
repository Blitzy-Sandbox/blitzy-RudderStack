// Package schema_test provides comprehensive tests for the common JSON Schema
// definition applied to all events from connected sources, as defined in
// protocols/schema/common_schema.go. This validates the base event structure
// enforcement for Sprint 5-7, E-020.
package schema_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"

	"github.com/rudderlabs/rudder-server/protocols/schema"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// validBaseEvent returns a minimal well-formed event containing all required
// base fields. Tests that extend it can add or remove fields as needed.
func validBaseEvent() map[string]any {
	return map[string]any{
		"messageId": "msg-123",
		"type":      "track",
		"timestamp": "2024-01-01T00:00:00Z",
	}
}

// hasConstraint returns true if at least one ValidationError in the slice has
// the given constraint value.
func hasConstraint(errs []schema.ValidationError, constraint string) bool {
	for _, e := range errs {
		if e.Constraint == constraint {
			return true
		}
	}
	return false
}

// hasMessageContaining returns true if at least one ValidationError in the
// slice has a Message that contains the given substring.
func hasMessageContaining(errs []schema.ValidationError, sub string) bool {
	for _, e := range errs {
		if containsSubstring(e.Message, sub) {
			return true
		}
	}
	return false
}

// containsSubstring is a simple substring check helper.
func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ===========================================================================
// Phase 1: Test Common Event Schema Definition
// ===========================================================================

func Test_CommonEventSchema_NotEmpty(t *testing.T) {
	// CommonEventSchema must be a non-nil, non-empty byte slice containing a
	// valid JSON Schema document.
	require.NotNil(t, schema.CommonEventSchema, "CommonEventSchema should not be nil")
	require.NotEmpty(t, schema.CommonEventSchema, "CommonEventSchema should not be empty")
}

func Test_CommonEventSchema_Compilable(t *testing.T) {
	// The common schema must successfully compile as a valid JSON Schema
	// draft-07 document using the CompileSchema engine.
	compiled, err := schema.CompileSchema(schema.CommonEventSchema)
	require.NoError(t, err, "CommonEventSchema must compile without error")
	require.NotNil(t, compiled, "compiled schema must not be nil")
}

func Test_CommonEventSchema_IsDraft07(t *testing.T) {
	// Parse the raw schema bytes and verify the $schema field declares draft-07.
	var parsed map[string]any
	err := jsonrs.Unmarshal(schema.CommonEventSchema, &parsed)
	require.NoError(t, err, "CommonEventSchema must be valid JSON")

	schemaURI, ok := parsed["$schema"]
	require.True(t, ok, "$schema key must be present")
	assert.Equal(t, "http://json-schema.org/draft-07/schema#", schemaURI,
		"$schema must declare JSON Schema draft-07")
}

// ===========================================================================
// Phase 2: Test Required Base Fields
// ===========================================================================

func Test_CommonSchema_ValidEvent(t *testing.T) {
	// A well-formed event with all required base fields should pass with zero
	// validation errors.
	event := validBaseEvent()

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "valid event should produce zero validation errors")
}

func Test_CommonSchema_MissingMessageId(t *testing.T) {
	event := map[string]any{
		"type":      "track",
		"timestamp": "2024-01-01T00:00:00Z",
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "missing messageId should produce validation errors")
	assert.True(t, hasConstraint(errs, "required"),
		"at least one error should have constraint 'required'")
	assert.True(t, hasMessageContaining(errs, "messageId"),
		"at least one error message should reference 'messageId'")
}

func Test_CommonSchema_MissingType(t *testing.T) {
	event := map[string]any{
		"messageId": "msg-456",
		"timestamp": "2024-01-01T00:00:00Z",
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "missing type should produce validation errors")
	assert.True(t, hasConstraint(errs, "required"),
		"at least one error should have constraint 'required'")
	assert.True(t, hasMessageContaining(errs, "type"),
		"at least one error message should reference 'type'")
}

func Test_CommonSchema_MissingTimestamp(t *testing.T) {
	event := map[string]any{
		"messageId": "msg-789",
		"type":      "track",
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "missing timestamp should produce validation errors")
	assert.True(t, hasConstraint(errs, "required"),
		"at least one error should have constraint 'required'")
	assert.True(t, hasMessageContaining(errs, "timestamp"),
		"at least one error message should reference 'timestamp'")
}

func Test_CommonSchema_AllRequiredFieldsMissing(t *testing.T) {
	// An empty event must fail with at least one validation error per missing
	// required field: messageId, type, timestamp.
	event := map[string]any{}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(errs), 1,
		"empty event should produce validation errors for missing required fields")

	// All three required fields should be mentioned.
	allMessages := ""
	for _, e := range errs {
		allMessages += " " + e.Message
	}
	assert.True(t, hasMessageContaining(errs, "messageId") ||
		containsSubstring(allMessages, "messageId"),
		"errors should reference missing 'messageId'")
	assert.True(t, hasMessageContaining(errs, "type") ||
		containsSubstring(allMessages, "type"),
		"errors should reference missing 'type'")
	assert.True(t, hasMessageContaining(errs, "timestamp") ||
		containsSubstring(allMessages, "timestamp"),
		"errors should reference missing 'timestamp'")
}

// ===========================================================================
// Phase 3: Test Type Field Validation
// ===========================================================================

func Test_CommonSchema_ValidEventTypes(t *testing.T) {
	// The common schema defines an enum for the "type" field matching the
	// RudderStack/Segment event specification.
	validTypes := []string{"track", "identify", "page", "screen", "group", "alias"}

	for _, eventType := range validTypes {
		t.Run(eventType, func(t *testing.T) {
			event := map[string]any{
				"messageId": "msg-type-" + eventType,
				"type":      eventType,
				"timestamp": "2024-06-15T10:30:00Z",
			}
			errs, err := schema.Validate(schema.CommonEventSchema, event)
			require.NoError(t, err)
			assert.Empty(t, errs, "event type %q should be valid", eventType)
		})
	}
}

func Test_CommonSchema_TypeMustBeString(t *testing.T) {
	// An integer value for `type` should fail because the schema requires a
	// string with an enum constraint.
	event := map[string]any{
		"messageId": "msg-type-int",
		"type":      42,
		"timestamp": "2024-01-01T00:00:00Z",
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "integer type value should fail validation")

	// Should fail on either "type" or "enum" constraint (library-dependent).
	foundTypeOrEnum := hasConstraint(errs, "type") || hasConstraint(errs, "enum")
	assert.True(t, foundTypeOrEnum,
		"validation error should be for 'type' or 'enum' constraint, got: %+v", errs)
}

func Test_CommonSchema_InvalidTypeEnum(t *testing.T) {
	// A string value that is not one of the allowed enum types should fail.
	event := map[string]any{
		"messageId": "msg-bad-type",
		"type":      "invalid_type",
		"timestamp": "2024-01-01T00:00:00Z",
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "invalid enum value should fail validation")
	assert.True(t, hasConstraint(errs, "enum"),
		"validation error should have constraint 'enum'")
}

// ===========================================================================
// Phase 4: Test Additional Common Properties
// ===========================================================================

func Test_CommonSchema_WithContext(t *testing.T) {
	event := validBaseEvent()
	event["context"] = map[string]any{
		"library": map[string]any{"name": "analytics-go", "version": "1.0.0"},
		"os":      map[string]any{"name": "linux"},
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "event with context object should pass")
}

func Test_CommonSchema_WithProperties(t *testing.T) {
	event := validBaseEvent()
	event["properties"] = map[string]any{
		"product_id": "abc-123",
		"revenue":    29.99,
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "event with properties object should pass")
}

func Test_CommonSchema_WithAnonymousId(t *testing.T) {
	event := validBaseEvent()
	event["anonymousId"] = "anon-001"

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "event with anonymousId should pass")
}

func Test_CommonSchema_WithUserId(t *testing.T) {
	event := validBaseEvent()
	event["userId"] = "user-42"

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "event with userId should pass")
}

func Test_CommonSchema_AdditionalPropertiesAllowed(t *testing.T) {
	// The common schema sets additionalProperties: true so that custom event
	// structures are not rejected.
	event := validBaseEvent()
	event["customField1"] = "hello"
	event["customField2"] = 42
	event["customField3"] = map[string]any{"nested": true}
	event["customField4"] = []any{1, 2, 3}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs,
		"event with additional non-standard properties should pass since additionalProperties is true")
}

// ===========================================================================
// Phase 5: Test Relationship with Event-Specific Schemas
// ===========================================================================

func Test_CommonSchema_AppliedBeforeEventSchema(t *testing.T) {
	// Demonstrate that common schema validation happens independently of any
	// event-specific (tracking plan) schema. We validate the same event against
	// both schemas and verify each produces its own set of results.

	// A tracking-plan-specific schema that requires a "properties.product_id" field.
	trackSchema := []byte(`{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"required": ["properties"],
		"properties": {
			"properties": {
				"type": "object",
				"required": ["product_id"],
				"properties": {
					"product_id": {"type": "string"}
				}
			}
		},
		"additionalProperties": true
	}`)

	event := validBaseEvent() // has messageId, type, timestamp but no "properties"

	// Common schema validation: should pass (all required base fields present).
	commonErrs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, commonErrs, "common schema should pass for valid base event")

	// Event-specific schema validation: should fail (missing "properties").
	specificErrs, err := schema.Validate(trackSchema, event)
	require.NoError(t, err)
	assert.NotEmpty(t, specificErrs,
		"event-specific schema should fail due to missing 'properties'")
}

func Test_CommonSchema_ValidateAndEventSpecificSchema(t *testing.T) {
	// An event that fails the common schema should still be independently
	// testable against an event-specific schema. The two validation results
	// are independent.

	// Event missing messageId (fails common) but has properties (could pass specific).
	event := map[string]any{
		"type":      "track",
		"timestamp": "2024-01-01T00:00:00Z",
		"properties": map[string]any{
			"product_id": "abc-123",
		},
	}

	// Common schema: should fail due to missing messageId.
	commonErrs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.NotEmpty(t, commonErrs, "common schema should fail for missing messageId")

	// Event-specific schema: only cares about "properties.product_id".
	specificSchema := []byte(`{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"required": ["properties"],
		"properties": {
			"properties": {
				"type": "object",
				"required": ["product_id"],
				"properties": {
					"product_id": {"type": "string"}
				}
			}
		},
		"additionalProperties": true
	}`)

	specificErrs, err := schema.Validate(specificSchema, event)
	require.NoError(t, err)
	assert.Empty(t, specificErrs,
		"event-specific schema should pass — product_id is present")
}

// ===========================================================================
// Phase 6: Test ValidateCommonSchema Helper Function
// ===========================================================================

func Test_ValidateCommonSchema_ValidEvent(t *testing.T) {
	event := validBaseEvent()

	errs, err := schema.ValidateCommonSchema(event)
	require.NoError(t, err)
	assert.Empty(t, errs, "ValidateCommonSchema should return no errors for valid event")
}

func Test_ValidateCommonSchema_InvalidEvent(t *testing.T) {
	// Event missing "type" — should fail.
	event := map[string]any{
		"messageId": "msg-helper-invalid",
		"timestamp": "2024-01-01T00:00:00Z",
	}

	errs, err := schema.ValidateCommonSchema(event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "ValidateCommonSchema should return errors for invalid event")
	assert.True(t, hasConstraint(errs, "required"),
		"error constraint should be 'required'")
}

func Test_ValidateCommonSchema_NilEvent(t *testing.T) {
	// Passing nil should return a non-nil error (not a validation error, but a
	// programming error caught by ValidateWithCompiled's nil guard).
	errs, err := schema.ValidateCommonSchema(nil)
	require.Error(t, err, "ValidateCommonSchema(nil) should return an error")
	require.Nil(t, errs, "errors slice should be nil when an error is returned")
}

// ===========================================================================
// Phase 6b: Test GetCompiledCommonSchema
// ===========================================================================

func Test_GetCompiledCommonSchema_ReturnsNonNil(t *testing.T) {
	compiled, err := schema.GetCompiledCommonSchema()
	require.NoError(t, err, "GetCompiledCommonSchema should not return an error")
	require.NotNil(t, compiled, "compiled common schema should not be nil")
}

func Test_GetCompiledCommonSchema_UsableForValidation(t *testing.T) {
	compiled, err := schema.GetCompiledCommonSchema()
	require.NoError(t, err)
	require.NotNil(t, compiled)

	// Use the compiled schema directly with ValidateWithCompiled.
	event := validBaseEvent()
	errs, err := schema.ValidateWithCompiled(compiled, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "compiled common schema should validate a correct event")
}

// ===========================================================================
// Phase 7: Edge Cases
// ===========================================================================

func Test_CommonSchema_MinimalValidEvent(t *testing.T) {
	// The absolute minimum valid event has only the three required fields.
	event := map[string]any{
		"messageId": "min-msg",
		"type":      "identify",
		"timestamp": "2025-12-31T23:59:59Z",
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "minimal event with only required fields should pass")
}

func Test_CommonSchema_RichValidEvent(t *testing.T) {
	// A full event with all common properties populated should pass.
	event := map[string]any{
		"messageId":         "rich-msg-001",
		"type":              "track",
		"timestamp":         "2024-06-15T14:30:00.123Z",
		"anonymousId":       "anon-rich-001",
		"userId":            "user-rich-42",
		"event":             "Product Viewed",
		"channel":           "web",
		"originalTimestamp":  "2024-06-15T14:29:59.999Z",
		"sentAt":            "2024-06-15T14:30:00.050Z",
		"receivedAt":        "2024-06-15T14:30:00.200Z",
		"context": map[string]any{
			"library": map[string]any{"name": "analytics.js", "version": "2.0.0"},
			"page":    map[string]any{"url": "https://example.com/product/42"},
			"ip":      "10.0.0.1",
		},
		"properties": map[string]any{
			"product_id": "prod-42",
			"name":       "Widget",
			"price":      9.99,
		},
		"traits": map[string]any{
			"name":  "Jane Doe",
			"email": "jane@example.com",
		},
		"integrations": map[string]any{
			"All":       true,
			"Amplitude": false,
		},
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "rich event with all common properties should pass")
}

func Test_CommonSchema_TimestampFormat(t *testing.T) {
	// The common schema enforces format: "date-time" on the timestamp field.
	// JSON Schema draft-07 validates format by default in santhosh-tekuri/jsonschema v5.

	t.Run("valid_datetime", func(t *testing.T) {
		event := map[string]any{
			"messageId": "ts-valid",
			"type":      "track",
			"timestamp": "2024-01-01T00:00:00Z",
		}
		errs, err := schema.Validate(schema.CommonEventSchema, event)
		require.NoError(t, err)
		assert.Empty(t, errs, "ISO 8601 / RFC 3339 timestamp should pass")
	})

	t.Run("valid_datetime_with_offset", func(t *testing.T) {
		event := map[string]any{
			"messageId": "ts-offset",
			"type":      "track",
			"timestamp": "2024-06-15T14:30:00+05:30",
		}
		errs, err := schema.Validate(schema.CommonEventSchema, event)
		require.NoError(t, err)
		assert.Empty(t, errs, "RFC 3339 timestamp with timezone offset should pass")
	})

	t.Run("invalid_datetime", func(t *testing.T) {
		event := map[string]any{
			"messageId": "ts-invalid",
			"type":      "track",
			"timestamp": "not-a-date",
		}
		errs, err := schema.Validate(schema.CommonEventSchema, event)
		require.NoError(t, err)
		require.NotEmpty(t, errs, "'not-a-date' should fail date-time format validation")
		assert.True(t, hasConstraint(errs, "format"),
			"validation error should have constraint 'format'")
	})
}

func Test_CommonSchema_NilMessageId(t *testing.T) {
	// messageId is required to be a string. Setting it to nil effectively makes
	// it JSON null, which should fail the "type": "string" constraint.
	event := map[string]any{
		"messageId": nil,
		"type":      "track",
		"timestamp": "2024-01-01T00:00:00Z",
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "nil messageId should fail validation")
	// Should fail on the "type" constraint (expected string, got null).
	assert.True(t, hasConstraint(errs, "type"),
		"validation error should have constraint 'type' for nil messageId")
}

func Test_CommonSchema_IntegerMessageId(t *testing.T) {
	// messageId must be a string. An integer value should fail type validation.
	event := map[string]any{
		"messageId": 12345,
		"type":      "track",
		"timestamp": "2024-01-01T00:00:00Z",
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "integer messageId should fail type validation")
	assert.True(t, hasConstraint(errs, "type"),
		"error constraint should be 'type'")
}

func Test_CommonSchema_ContextWrongType(t *testing.T) {
	// context is defined as an object. Passing a string should fail.
	event := validBaseEvent()
	event["context"] = "not-an-object"

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "string context should fail object type validation")
	assert.True(t, hasConstraint(errs, "type"),
		"error constraint should be 'type'")
}

func Test_CommonSchema_PropertiesWrongType(t *testing.T) {
	// properties is defined as an object. Passing an array should fail.
	event := validBaseEvent()
	event["properties"] = []any{"a", "b"}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "array properties should fail object type validation")
	assert.True(t, hasConstraint(errs, "type"),
		"error constraint should be 'type'")
}

func Test_CommonSchema_OriginalTimestampInvalidFormat(t *testing.T) {
	// originalTimestamp also has format: "date-time". Invalid values should fail.
	event := validBaseEvent()
	event["originalTimestamp"] = "yesterday"

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "invalid originalTimestamp should fail format validation")
	assert.True(t, hasConstraint(errs, "format"),
		"error constraint should be 'format'")
}

func Test_CommonSchema_SentAtInvalidFormat(t *testing.T) {
	// sentAt also has format: "date-time".
	event := validBaseEvent()
	event["sentAt"] = "noon"

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "invalid sentAt should fail format validation")
	assert.True(t, hasConstraint(errs, "format"),
		"error constraint should be 'format'")
}

func Test_CommonSchema_ReceivedAtInvalidFormat(t *testing.T) {
	// receivedAt also has format: "date-time".
	event := validBaseEvent()
	event["receivedAt"] = "last-tuesday"

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "invalid receivedAt should fail format validation")
	assert.True(t, hasConstraint(errs, "format"),
		"error constraint should be 'format'")
}

func Test_CommonSchema_MultipleViolations(t *testing.T) {
	// An event with multiple constraint violations should return all errors,
	// not just the first one encountered.
	event := map[string]any{
		// Missing messageId (required)
		"type":      42,          // wrong type (should be string/enum)
		"timestamp": "not-a-ts",  // invalid format
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(errs), 2,
		"multiple violations should produce multiple validation errors")
}

func Test_CommonSchema_EmptyStringFields(t *testing.T) {
	// The common schema requires messageId and type to be strings but does not
	// enforce minLength. Empty strings should still pass the "required" and
	// "type" constraints (but may fail "enum" for type).
	t.Run("empty_messageId", func(t *testing.T) {
		event := map[string]any{
			"messageId": "",
			"type":      "track",
			"timestamp": "2024-01-01T00:00:00Z",
		}
		errs, err := schema.Validate(schema.CommonEventSchema, event)
		require.NoError(t, err)
		// Empty string is still a valid string — should pass messageId constraint.
		assert.Empty(t, errs, "empty messageId string should pass (required + type: string)")
	})

	t.Run("empty_type", func(t *testing.T) {
		event := map[string]any{
			"messageId": "msg-empty-type",
			"type":      "",
			"timestamp": "2024-01-01T00:00:00Z",
		}
		errs, err := schema.Validate(schema.CommonEventSchema, event)
		require.NoError(t, err)
		// Empty string is NOT in the enum list, so this should fail.
		require.NotEmpty(t, errs, "empty type string should fail enum validation")
		assert.True(t, hasConstraint(errs, "enum"),
			"error constraint should be 'enum'")
	})
}

func Test_CommonSchema_BoolTimestamp(t *testing.T) {
	// timestamp must be a string. A boolean should fail type validation.
	event := map[string]any{
		"messageId": "msg-bool-ts",
		"type":      "track",
		"timestamp": true,
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "boolean timestamp should fail type validation")
	assert.True(t, hasConstraint(errs, "type"),
		"error constraint should be 'type'")
}

func Test_CommonSchema_LargeEvent(t *testing.T) {
	// Validate a large event with many additional properties to verify no
	// panics or performance issues.
	event := validBaseEvent()
	for i := 0; i < 200; i++ {
		event[fmt.Sprintf("field_%d", i)] = fmt.Sprintf("value_%d", i)
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "large event with additional properties should pass")
}

func Test_CommonSchema_UnicodeProperties(t *testing.T) {
	// Events with Unicode characters in names and values should validate.
	event := validBaseEvent()
	event["名前"] = "テスト"
	event["emoji"] = "🎉🚀"
	event["properties"] = map[string]any{
		"描述": "这是一个测试事件",
	}

	errs, err := schema.Validate(schema.CommonEventSchema, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "event with Unicode properties should pass")
}
