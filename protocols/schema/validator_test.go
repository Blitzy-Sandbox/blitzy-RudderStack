// Package schema_test provides comprehensive black-box unit tests for the
// JSON Schema draft-07 validation engine defined in protocols/schema/validator.go.
// This file is part of Sprint 5-7, E-020: Protocols and Tracking Plan Enforcement.
//
// It covers 12 test phases:
//  1. CompileSchema function
//  2. Type enforcement (string, integer, number, boolean, array, object, null, any, date-time)
//  3. Required fields
//  4. Regex patterns
//  5. Nested objects
//  6. Enum values
//  7. Additional keywords (minLength, maxLength, minimum, maximum, minItems, maxItems, additionalProperties)
//  8. ValidationError structure fields
//  9. ValidateBatch function
//  10. ValidateWithCompiled function
//  11. Edge cases
//  12. Common schema integration
package schema_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-server/protocols/schema"
)

// ===========================================================================
// Phase 1: Test CompileSchema Function
// ===========================================================================

func Test_CompileSchema_ValidSchema(t *testing.T) {
	schemaJSON := []byte(`{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"},
			"age":  {"type": "integer"}
		}
	}`)

	compiled, err := schema.CompileSchema(schemaJSON)
	require.NoError(t, err, "valid schema should compile without error")
	require.NotNil(t, compiled, "compiled schema must not be nil for valid input")
}

func Test_CompileSchema_InvalidJSON(t *testing.T) {
	malformed := []byte(`{invalid}`)

	compiled, err := schema.CompileSchema(malformed)
	require.Error(t, err, "malformed JSON should return error")
	require.Nil(t, compiled, "compiled schema must be nil for invalid JSON")
}

func Test_CompileSchema_InvalidSchema(t *testing.T) {
	// "unknowntype" is not a valid JSON Schema type; the meta-schema rejects it.
	invalidSchema := []byte(`{"type": "unknowntype"}`)

	compiled, err := schema.CompileSchema(invalidSchema)
	require.Error(t, err, "semantically invalid schema should return error")
	require.Nil(t, compiled, "compiled schema must be nil for invalid schema")
}

func Test_CompileSchema_EmptySchema(t *testing.T) {
	// Per JSON Schema draft-07, an empty schema {} validates everything.
	emptySchema := []byte(`{}`)

	compiled, err := schema.CompileSchema(emptySchema)
	require.NoError(t, err, "empty schema should compile without error")
	require.NotNil(t, compiled, "compiled schema must not be nil for empty schema")
}

func Test_CompileSchema_NilInput(t *testing.T) {
	compiled, err := schema.CompileSchema(nil)
	require.Error(t, err, "nil input should return error")
	require.Nil(t, compiled, "compiled schema must be nil for nil input")
}

// ===========================================================================
// Phase 2: Test Validate Function — Type Enforcement
// ===========================================================================

func Test_Validate_StringType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["val"],
		"properties": {
			"val": {"type": "string"}
		}
	}`)

	t.Run("valid_string", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": "hello"})
		require.NoError(t, err)
		assert.Empty(t, errs, "valid string should produce zero errors")
	})

	t.Run("integer_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": 42})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "integer should fail string type check")
		assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")
	})
}

func Test_Validate_IntegerType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["val"],
		"properties": {
			"val": {"type": "integer"}
		}
	}`)

	t.Run("valid_integer", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": 42})
		require.NoError(t, err)
		assert.Empty(t, errs, "valid integer should produce zero errors")
	})

	t.Run("float_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": 3.5})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "float should fail integer type check")
		assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")
	})

	t.Run("string_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": "42"})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "string should fail integer type check")
		assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")
	})
}

func Test_Validate_NumberType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["val"],
		"properties": {
			"val": {"type": "number"}
		}
	}`)

	t.Run("integer_passes", func(t *testing.T) {
		// Integers are valid numbers per JSON Schema.
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": 42})
		require.NoError(t, err)
		assert.Empty(t, errs, "integer should pass number type check")
	})

	t.Run("float_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": 3.14})
		require.NoError(t, err)
		assert.Empty(t, errs, "float should pass number type check")
	})

	t.Run("string_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": "3.14"})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "string should fail number type check")
		assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")
	})
}

func Test_Validate_BooleanType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["val"],
		"properties": {
			"val": {"type": "boolean"}
		}
	}`)

	t.Run("true_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": true})
		require.NoError(t, err)
		assert.Empty(t, errs, "true should pass boolean type check")
	})

	t.Run("false_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": false})
		require.NoError(t, err)
		assert.Empty(t, errs, "false should pass boolean type check")
	})

	t.Run("string_true_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": "true"})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "string 'true' should fail boolean type check")
		assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")
	})
}

func Test_Validate_ArrayType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["val"],
		"properties": {
			"val": {"type": "array"}
		}
	}`)

	t.Run("empty_array_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": []any{}})
		require.NoError(t, err)
		assert.Empty(t, errs, "empty array should pass array type check")
	})

	t.Run("populated_array_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": []any{1, "two", true}})
		require.NoError(t, err)
		assert.Empty(t, errs, "populated array should pass array type check")
	})

	t.Run("string_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": "not-array"})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "string should fail array type check")
		assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")
	})
}

func Test_Validate_ObjectType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["val"],
		"properties": {
			"val": {"type": "object"}
		}
	}`)

	t.Run("empty_object_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": map[string]any{}})
		require.NoError(t, err)
		assert.Empty(t, errs, "empty object should pass object type check")
	})

	t.Run("populated_object_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{
			"val": map[string]any{"key": "value"},
		})
		require.NoError(t, err)
		assert.Empty(t, errs, "populated object should pass object type check")
	})

	t.Run("array_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": []any{1, 2}})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "array should fail object type check")
		assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")
	})
}

func Test_Validate_NullType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["val"],
		"properties": {
			"val": {"type": "null"}
		}
	}`)

	t.Run("null_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": nil})
		require.NoError(t, err)
		assert.Empty(t, errs, "nil (null) should pass null type check")
	})

	t.Run("empty_string_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": ""})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "empty string should fail null type check")
		assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")
	})
}

func Test_Validate_AnyType(t *testing.T) {
	// Schema with no type constraint on property — any value is valid.
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"val": {}
		}
	}`)

	testCases := []struct {
		name  string
		value any
	}{
		{"string", "hello"},
		{"integer", 42},
		{"float", 3.14},
		{"boolean", true},
		{"null", nil},
		{"array", []any{1, 2, 3}},
		{"object", map[string]any{"key": "val"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs, err := schema.Validate(schemaJSON, map[string]any{"val": tc.value})
			require.NoError(t, err)
			assert.Empty(t, errs, "any type should accept %s value", tc.name)
		})
	}
}

func Test_Validate_DateTimeFormat(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["ts"],
		"properties": {
			"ts": {"type": "string", "format": "date-time"}
		}
	}`)

	t.Run("valid_rfc3339", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"ts": "2024-01-15T10:30:00Z"})
		require.NoError(t, err)
		assert.Empty(t, errs, "valid RFC3339 datetime should pass")
	})

	t.Run("valid_with_offset", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"ts": "2024-06-15T14:30:00+05:30"})
		require.NoError(t, err)
		assert.Empty(t, errs, "RFC3339 with timezone offset should pass")
	})

	t.Run("valid_with_millis", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"ts": "2024-01-15T10:30:00.123Z"})
		require.NoError(t, err)
		assert.Empty(t, errs, "RFC3339 with milliseconds should pass")
	})

	t.Run("invalid_datetime", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"ts": "not-a-datetime"})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "invalid datetime string should fail format validation")
		assert.True(t, hasConstraint(errs, "format"), "constraint should be 'format'")
	})

	t.Run("date_only_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"ts": "2024-01-15"})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "date-only string should fail date-time format")
		assert.True(t, hasConstraint(errs, "format"), "constraint should be 'format'")
	})
}

// ===========================================================================
// Phase 3: Test Validate Function — Required Fields
// ===========================================================================

func Test_Validate_RequiredFieldPresent(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	errs, err := schema.Validate(schemaJSON, map[string]any{"name": "Alice"})
	require.NoError(t, err)
	assert.Empty(t, errs, "event with required field present should pass")
}

func Test_Validate_RequiredFieldMissing(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name", "email"],
		"properties": {
			"name":  {"type": "string"},
			"email": {"type": "string"}
		}
	}`)

	errs, err := schema.Validate(schemaJSON, map[string]any{"name": "Alice"})
	require.NoError(t, err)
	require.NotEmpty(t, errs, "missing required field should produce validation errors")
	assert.True(t, hasConstraint(errs, "required"), "constraint should be 'required'")
	assert.True(t, hasMessageContaining(errs, "email"),
		"error message should reference the missing field 'email'")
}

func Test_Validate_RequiredFieldEmpty(t *testing.T) {
	// required checks presence, not emptiness — an empty string satisfies "required".
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	errs, err := schema.Validate(schemaJSON, map[string]any{"name": ""})
	require.NoError(t, err)
	assert.Empty(t, errs, "empty string should satisfy 'required' constraint")
}

func Test_Validate_MultipleRequiredFieldsMissing(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name", "email", "age"],
		"properties": {
			"name":  {"type": "string"},
			"email": {"type": "string"},
			"age":   {"type": "integer"}
		}
	}`)

	errs, err := schema.Validate(schemaJSON, map[string]any{})
	require.NoError(t, err)
	require.NotEmpty(t, errs, "missing multiple required fields should produce errors")

	// The jsonschema library reports all missing required fields in a single
	// error message. Verify all missing fields are referenced.
	assert.True(t, hasConstraint(errs, "required"), "constraint should be 'required'")
	combinedMessages := ""
	for _, e := range errs {
		combinedMessages += " " + e.Message
	}
	assert.True(t, containsSubstring(combinedMessages, "name"),
		"error should reference missing 'name'")
	assert.True(t, containsSubstring(combinedMessages, "email"),
		"error should reference missing 'email'")
	assert.True(t, containsSubstring(combinedMessages, "age"),
		"error should reference missing 'age'")
}

// ===========================================================================
// Phase 4: Test Validate Function — Regex Patterns
// ===========================================================================

func Test_Validate_PatternMatch(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["code"],
		"properties": {
			"code": {"type": "string", "pattern": "^[a-z]+$"}
		}
	}`)

	errs, err := schema.Validate(schemaJSON, map[string]any{"code": "abcdef"})
	require.NoError(t, err)
	assert.Empty(t, errs, "lowercase string should match pattern ^[a-z]+$")
}

func Test_Validate_PatternMismatch(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["code"],
		"properties": {
			"code": {"type": "string", "pattern": "^[a-z]+$"}
		}
	}`)

	errs, err := schema.Validate(schemaJSON, map[string]any{"code": "UPPER"})
	require.NoError(t, err)
	require.NotEmpty(t, errs, "uppercase string should fail pattern ^[a-z]+$")
	assert.True(t, hasConstraint(errs, "pattern"), "constraint should be 'pattern'")
}

func Test_Validate_EmailPattern(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["email"],
		"properties": {
			"email": {"type": "string", "format": "email"}
		}
	}`)

	t.Run("valid_email", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"email": "user@example.com"})
		require.NoError(t, err)
		assert.Empty(t, errs, "valid email should pass format check")
	})

	t.Run("invalid_email", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"email": "not-an-email"})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "invalid email should fail format check")
		assert.True(t, hasConstraint(errs, "format"), "constraint should be 'format'")
	})
}

// ===========================================================================
// Phase 5: Test Validate Function — Nested Objects
// ===========================================================================

func Test_Validate_NestedObjectValid(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["address"],
		"properties": {
			"address": {
				"type": "object",
				"required": ["city"],
				"properties": {
					"city": {"type": "string"}
				}
			}
		}
	}`)

	event := map[string]any{
		"address": map[string]any{
			"city": "San Francisco",
		},
	}

	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "valid nested object should pass validation")
}

func Test_Validate_NestedObjectInvalidType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["address"],
		"properties": {
			"address": {
				"type": "object",
				"required": ["city"],
				"properties": {
					"city": {"type": "string"}
				}
			}
		}
	}`)

	event := map[string]any{
		"address": map[string]any{
			"city": 12345,
		},
	}

	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "nested field with wrong type should fail")
	assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")

	// Verify the FieldPath includes the full path to the nested field.
	found := false
	for _, e := range errs {
		if containsSubstring(e.FieldPath, "address") && containsSubstring(e.FieldPath, "city") {
			found = true
			break
		}
	}
	assert.True(t, found, "FieldPath should include path to nested field (address.city)")
}

func Test_Validate_NestedObjectRequiredField(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["address"],
		"properties": {
			"address": {
				"type": "object",
				"required": ["city", "zip"],
				"properties": {
					"city": {"type": "string"},
					"zip":  {"type": "string"}
				}
			}
		}
	}`)

	event := map[string]any{
		"address": map[string]any{
			"city": "Portland",
		},
	}

	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "missing nested required field should produce errors")
	assert.True(t, hasConstraint(errs, "required"), "constraint should be 'required'")
	assert.True(t, hasMessageContaining(errs, "zip"),
		"error message should reference missing nested field 'zip'")
}

func Test_Validate_DeepNesting(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["level1"],
		"properties": {
			"level1": {
				"type": "object",
				"required": ["level2"],
				"properties": {
					"level2": {
						"type": "object",
						"required": ["level3"],
						"properties": {
							"level3": {"type": "string"}
						}
					}
				}
			}
		}
	}`)

	t.Run("valid_deep_nesting", func(t *testing.T) {
		event := map[string]any{
			"level1": map[string]any{
				"level2": map[string]any{
					"level3": "deep-value",
				},
			},
		}
		errs, err := schema.Validate(schemaJSON, event)
		require.NoError(t, err)
		assert.Empty(t, errs, "valid deep nesting should pass")
	})

	t.Run("invalid_deep_type", func(t *testing.T) {
		event := map[string]any{
			"level1": map[string]any{
				"level2": map[string]any{
					"level3": 42,
				},
			},
		}
		errs, err := schema.Validate(schemaJSON, event)
		require.NoError(t, err)
		require.NotEmpty(t, errs, "wrong type at deep level should fail")
		assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")

		found := false
		for _, e := range errs {
			if containsSubstring(e.FieldPath, "level1") &&
				containsSubstring(e.FieldPath, "level2") &&
				containsSubstring(e.FieldPath, "level3") {
				found = true
				break
			}
		}
		assert.True(t, found, "FieldPath should include full deep path")
	})
}

// ===========================================================================
// Phase 6: Test Validate Function — Enum Values
// ===========================================================================

func Test_Validate_EnumValid(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["eventType"],
		"properties": {
			"eventType": {"enum": ["track", "identify", "page"]}
		}
	}`)

	for _, val := range []string{"track", "identify", "page"} {
		t.Run(val, func(t *testing.T) {
			errs, err := schema.Validate(schemaJSON, map[string]any{"eventType": val})
			require.NoError(t, err)
			assert.Empty(t, errs, "valid enum value %q should pass", val)
		})
	}
}

func Test_Validate_EnumInvalid(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["eventType"],
		"properties": {
			"eventType": {"enum": ["track", "identify", "page"]}
		}
	}`)

	errs, err := schema.Validate(schemaJSON, map[string]any{"eventType": "invalid"})
	require.NoError(t, err)
	require.NotEmpty(t, errs, "invalid enum value should produce errors")
	assert.True(t, hasConstraint(errs, "enum"), "constraint should be 'enum'")
}

func Test_Validate_EnumMultipleTypes(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["val"],
		"properties": {
			"val": {"enum": ["active", 1, "inactive", 0]}
		}
	}`)

	t.Run("string_value", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": "active"})
		require.NoError(t, err)
		assert.Empty(t, errs, "string enum value should pass")
	})

	t.Run("integer_value", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": 1})
		require.NoError(t, err)
		assert.Empty(t, errs, "integer enum value should pass")
	})

	t.Run("invalid_value", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"val": "unknown"})
		require.NoError(t, err)
		require.NotEmpty(t, errs, "invalid enum value should fail")
		assert.True(t, hasConstraint(errs, "enum"), "constraint should be 'enum'")
	})
}

// ===========================================================================
// Phase 7: Test Validate Function — Additional Keywords
// ===========================================================================

func Test_Validate_MinLength(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"code": {"type": "string", "minLength": 3}
		}
	}`)

	t.Run("too_short", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"code": "ab"})
		require.NoError(t, err)
		require.NotEmpty(t, errs)
		assert.True(t, hasConstraint(errs, "minLength"), "constraint should be 'minLength'")
	})

	t.Run("adequate_length", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"code": "abc"})
		require.NoError(t, err)
		assert.Empty(t, errs, "string meeting minLength should pass")
	})
}

func Test_Validate_MaxLength(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"code": {"type": "string", "maxLength": 10}
		}
	}`)

	t.Run("too_long", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"code": "01234567890"})
		require.NoError(t, err)
		require.NotEmpty(t, errs)
		assert.True(t, hasConstraint(errs, "maxLength"), "constraint should be 'maxLength'")
	})

	t.Run("within_limit", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"code": "012345"})
		require.NoError(t, err)
		assert.Empty(t, errs, "string within maxLength should pass")
	})
}

func Test_Validate_Minimum(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"age": {"type": "number", "minimum": 0}
		}
	}`)

	t.Run("negative_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"age": -1})
		require.NoError(t, err)
		require.NotEmpty(t, errs)
		assert.True(t, hasConstraint(errs, "minimum"), "constraint should be 'minimum'")
	})

	t.Run("zero_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"age": 0})
		require.NoError(t, err)
		assert.Empty(t, errs, "zero should pass minimum 0")
	})
}

func Test_Validate_Maximum(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"score": {"type": "number", "maximum": 100}
		}
	}`)

	t.Run("exceeds_max", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"score": 150})
		require.NoError(t, err)
		require.NotEmpty(t, errs)
		assert.True(t, hasConstraint(errs, "maximum"), "constraint should be 'maximum'")
	})

	t.Run("within_max", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"score": 100})
		require.NoError(t, err)
		assert.Empty(t, errs, "value at maximum should pass")
	})
}

func Test_Validate_MinItems(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"tags": {"type": "array", "minItems": 1}
		}
	}`)

	t.Run("empty_array_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"tags": []any{}})
		require.NoError(t, err)
		require.NotEmpty(t, errs)
		assert.True(t, hasConstraint(errs, "minItems"), "constraint should be 'minItems'")
	})

	t.Run("populated_array_passes", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{"tags": []any{"a"}})
		require.NoError(t, err)
		assert.Empty(t, errs, "non-empty array should pass minItems 1")
	})
}

func Test_Validate_MaxItems(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"tags": {"type": "array", "maxItems": 5}
		}
	}`)

	t.Run("oversized_array_fails", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{
			"tags": []any{"a", "b", "c", "d", "e", "f"},
		})
		require.NoError(t, err)
		require.NotEmpty(t, errs)
		assert.True(t, hasConstraint(errs, "maxItems"), "constraint should be 'maxItems'")
	})

	t.Run("within_limit", func(t *testing.T) {
		errs, err := schema.Validate(schemaJSON, map[string]any{
			"tags": []any{"a", "b"},
		})
		require.NoError(t, err)
		assert.Empty(t, errs, "array within maxItems should pass")
	})
}

func Test_Validate_AdditionalPropertiesFalse(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": false
	}`)

	t.Run("extra_property_fails", func(t *testing.T) {
		event := map[string]any{"name": "Alice", "age": 30}
		errs, err := schema.Validate(schemaJSON, event)
		require.NoError(t, err)
		require.NotEmpty(t, errs, "extra properties should fail when additionalProperties=false")
		assert.True(t, hasConstraint(errs, "additionalProperties"),
			"constraint should be 'additionalProperties'")
	})

	t.Run("no_extra_passes", func(t *testing.T) {
		event := map[string]any{"name": "Alice"}
		errs, err := schema.Validate(schemaJSON, event)
		require.NoError(t, err)
		assert.Empty(t, errs, "known properties only should pass")
	})
}

func Test_Validate_AdditionalPropertiesTrue(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": true
	}`)

	event := map[string]any{"name": "Alice", "age": 30, "extra": true}
	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "extra properties should pass when additionalProperties=true")
}

// ===========================================================================
// Phase 8: Test ValidationError Structure
// ===========================================================================

func Test_ValidationError_FieldPath(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"address": {
				"type": "object",
				"properties": {
					"city": {"type": "string"}
				}
			}
		}
	}`)

	event := map[string]any{
		"address": map[string]any{
			"city": 123,
		},
	}

	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs)

	found := false
	for _, e := range errs {
		if e.FieldPath == "address.city" {
			found = true
			break
		}
	}
	assert.True(t, found, "FieldPath should be 'address.city', got paths: %v",
		fieldPaths(errs))
}

func Test_ValidationError_ExpectedType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	event := map[string]any{"name": 42}

	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs)

	found := false
	for _, e := range errs {
		if e.ExpectedType == "string" {
			found = true
			break
		}
	}
	assert.True(t, found, "ExpectedType should be 'string' for type violation")
}

func Test_ValidationError_ActualValue(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"count": {"type": "string"}
		}
	}`)

	event := map[string]any{"count": 42}

	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	require.NotEmpty(t, errs)

	// ActualValue should contain the string representation of the violation message.
	found := false
	for _, e := range errs {
		if e.ActualValue != "" {
			found = true
			break
		}
	}
	assert.True(t, found, "ActualValue should be populated for type violation")
}

func Test_ValidationError_Constraint(t *testing.T) {
	tests := []struct {
		name       string
		schemaJSON []byte
		event      map[string]any
		constraint string
	}{
		{
			name: "required",
			schemaJSON: []byte(`{
				"type": "object",
				"required": ["name"],
				"properties": {"name": {"type": "string"}}
			}`),
			event:      map[string]any{},
			constraint: "required",
		},
		{
			name: "type",
			schemaJSON: []byte(`{
				"type": "object",
				"properties": {"name": {"type": "string"}}
			}`),
			event:      map[string]any{"name": 42},
			constraint: "type",
		},
		{
			name: "pattern",
			schemaJSON: []byte(`{
				"type": "object",
				"properties": {"code": {"type": "string", "pattern": "^[a-z]+$"}}
			}`),
			event:      map[string]any{"code": "ABC"},
			constraint: "pattern",
		},
		{
			name: "enum",
			schemaJSON: []byte(`{
				"type": "object",
				"properties": {"status": {"enum": ["active", "inactive"]}}
			}`),
			event:      map[string]any{"status": "unknown"},
			constraint: "enum",
		},
		{
			name: "format",
			schemaJSON: []byte(`{
				"type": "object",
				"properties": {"ts": {"type": "string", "format": "date-time"}}
			}`),
			event:      map[string]any{"ts": "not-a-date"},
			constraint: "format",
		},
		{
			name: "minimum",
			schemaJSON: []byte(`{
				"type": "object",
				"properties": {"val": {"type": "number", "minimum": 0}}
			}`),
			event:      map[string]any{"val": -1},
			constraint: "minimum",
		},
		{
			name: "maximum",
			schemaJSON: []byte(`{
				"type": "object",
				"properties": {"val": {"type": "number", "maximum": 10}}
			}`),
			event:      map[string]any{"val": 20},
			constraint: "maximum",
		},
		{
			name: "minLength",
			schemaJSON: []byte(`{
				"type": "object",
				"properties": {"s": {"type": "string", "minLength": 5}}
			}`),
			event:      map[string]any{"s": "ab"},
			constraint: "minLength",
		},
		{
			name: "maxLength",
			schemaJSON: []byte(`{
				"type": "object",
				"properties": {"s": {"type": "string", "maxLength": 3}}
			}`),
			event:      map[string]any{"s": "abcdef"},
			constraint: "maxLength",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs, err := schema.Validate(tt.schemaJSON, tt.event)
			require.NoError(t, err)
			require.NotEmpty(t, errs, "expected validation error for constraint %q", tt.constraint)
			assert.True(t, hasConstraint(errs, tt.constraint),
				"constraint should be %q, got: %+v", tt.constraint, errs)
		})
	}
}

func Test_ValidationError_MultipleErrors(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name", "age"],
		"properties": {
			"name": {"type": "string"},
			"age": {"type": "integer"},
			"code": {"type": "string", "pattern": "^[a-z]+$"}
		}
	}`)

	// Missing required fields + wrong type for code.
	event := map[string]any{
		"code": "INVALID_UPPER",
	}

	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(errs), 2,
		"multiple violations should produce multiple errors, got %d", len(errs))
}

// fieldPaths is a test helper that extracts all FieldPath values from errors.
func fieldPaths(errs []schema.ValidationError) []string {
	paths := make([]string, len(errs))
	for i, e := range errs {
		paths[i] = e.FieldPath
	}
	return paths
}

// ===========================================================================
// Phase 9: Test ValidateBatch Function
// ===========================================================================

func Test_ValidateBatch_AllValid(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	events := []map[string]any{
		{"name": "Alice"},
		{"name": "Bob"},
		{"name": "Charlie"},
	}

	results, err := schema.ValidateBatch(schemaJSON, events)
	require.NoError(t, err)
	require.Len(t, results, 3, "results should have same length as events")
	for i, errs := range results {
		assert.Empty(t, errs, "event %d should pass validation", i)
	}
}

func Test_ValidateBatch_SomeInvalid(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	events := []map[string]any{
		{"name": "Alice"},
		{"other": "no-name"},
		{"name": "Charlie"},
	}

	results, err := schema.ValidateBatch(schemaJSON, events)
	require.NoError(t, err)
	require.Len(t, results, 3)
	assert.Empty(t, results[0], "first event should pass")
	assert.NotEmpty(t, results[1], "second event missing 'name' should fail")
	assert.Empty(t, results[2], "third event should pass")
}

func Test_ValidateBatch_AllInvalid(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	events := []map[string]any{
		{"other": 1},
		{"other": 2},
		{"other": 3},
	}

	results, err := schema.ValidateBatch(schemaJSON, events)
	require.NoError(t, err)
	require.Len(t, results, 3)
	for i, errs := range results {
		assert.NotEmpty(t, errs, "event %d should fail validation", i)
	}
}

func Test_ValidateBatch_EmptyBatch(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	results, err := schema.ValidateBatch(schemaJSON, []map[string]any{})
	require.NoError(t, err)
	assert.Empty(t, results, "empty batch should return empty results")
}

func Test_ValidateBatch_PrecompiledSchema(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	compiled, err := schema.CompileSchema(schemaJSON)
	require.NoError(t, err)
	require.NotNil(t, compiled)

	events := []map[string]any{
		{"name": "Alice"},
		{"other": "no-name"},
	}

	results, err := schema.ValidateBatchWithCompiled(compiled, events)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Empty(t, results[0], "first event should pass")
	assert.NotEmpty(t, results[1], "second event should fail")
}

// ===========================================================================
// Phase 10: Test ValidateWithCompiled Function
// ===========================================================================

func Test_ValidateWithCompiled_Success(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	compiled, err := schema.CompileSchema(schemaJSON)
	require.NoError(t, err)
	require.NotNil(t, compiled)

	errs, err := schema.ValidateWithCompiled(compiled, map[string]any{"name": "Alice"})
	require.NoError(t, err)
	assert.Empty(t, errs, "valid event with compiled schema should pass")
}

func Test_ValidateWithCompiled_Failure(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	compiled, err := schema.CompileSchema(schemaJSON)
	require.NoError(t, err)
	require.NotNil(t, compiled)

	// Use compiled schema for invalid event.
	compiledErrs, err := schema.ValidateWithCompiled(compiled, map[string]any{"other": "no-name"})
	require.NoError(t, err)
	require.NotEmpty(t, compiledErrs, "invalid event should fail with compiled schema")

	// Use raw Validate for the same event — results should match.
	rawErrs, err := schema.Validate(schemaJSON, map[string]any{"other": "no-name"})
	require.NoError(t, err)
	require.NotEmpty(t, rawErrs)

	// Both should produce errors with the same constraint.
	assert.Equal(t, len(compiledErrs), len(rawErrs),
		"compiled and raw Validate should produce the same number of errors")
	for i := range compiledErrs {
		if i < len(rawErrs) {
			assert.Equal(t, compiledErrs[i].Constraint, rawErrs[i].Constraint,
				"constraint at index %d should match", i)
		}
	}
}

func Test_ValidateWithCompiled_Reuse(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"},
			"age":  {"type": "integer"}
		}
	}`)

	compiled, err := schema.CompileSchema(schemaJSON)
	require.NoError(t, err)
	require.NotNil(t, compiled)

	// Validate multiple events with the same compiled schema.
	events := []map[string]any{
		{"name": "Alice", "age": 30},
		{"name": "Bob"},
		{"age": 25},
		{"name": "Charlie", "age": 40},
	}

	expectedValid := []bool{true, true, false, true}

	for i, event := range events {
		t.Run(fmt.Sprintf("event_%d", i), func(t *testing.T) {
			errs, err := schema.ValidateWithCompiled(compiled, event)
			require.NoError(t, err)
			if expectedValid[i] {
				assert.Empty(t, errs, "event %d should pass", i)
			} else {
				assert.NotEmpty(t, errs, "event %d should fail (missing required 'name')", i)
			}
		})
	}
}

// ===========================================================================
// Phase 11: Edge Cases
// ===========================================================================

func Test_Validate_EmptyEvent(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name", "email"],
		"properties": {
			"name":  {"type": "string"},
			"email": {"type": "string"}
		}
	}`)

	errs, err := schema.Validate(schemaJSON, map[string]any{})
	require.NoError(t, err)
	require.NotEmpty(t, errs, "empty event with required fields should fail")
	assert.True(t, hasConstraint(errs, "required"), "should report required constraint")
}

func Test_Validate_NilEvent(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"required": ["name"],
		"properties": {
			"name": {"type": "string"}
		}
	}`)

	_, err := schema.Validate(schemaJSON, nil)
	require.Error(t, err, "nil event should return an error")
}

func Test_Validate_EmptySchema(t *testing.T) {
	// Per JSON Schema draft-07, empty schema {} validates everything.
	schemaJSON := []byte(`{}`)

	event := map[string]any{
		"anything": "goes",
		"nested":   map[string]any{"deep": true},
		"number":   42,
	}

	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "empty schema should validate everything")
}

func Test_Validate_LargeEvent(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"additionalProperties": {"type": "string"}
	}`)

	// Build an event with 100+ properties.
	event := make(map[string]any, 120)
	for i := 0; i < 120; i++ {
		event[fmt.Sprintf("prop_%d", i)] = fmt.Sprintf("value_%d", i)
	}

	errs, err := schema.Validate(schemaJSON, event)
	require.NoError(t, err)
	assert.Empty(t, errs, "large event with correct types should pass")
}

func Test_Validate_UnicodeProperties(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"名前": {"type": "string"},
			"メール": {"type": "string"}
		}
	}`)

	t.Run("unicode_property_names_valid", func(t *testing.T) {
		event := map[string]any{
			"名前":   "太郎",
			"メール": "taro@example.com",
		}
		errs, err := schema.Validate(schemaJSON, event)
		require.NoError(t, err)
		assert.Empty(t, errs, "unicode property names and values should validate correctly")
	})

	t.Run("unicode_property_wrong_type", func(t *testing.T) {
		event := map[string]any{
			"名前": 42,
		}
		errs, err := schema.Validate(schemaJSON, event)
		require.NoError(t, err)
		require.NotEmpty(t, errs, "wrong type for unicode property should fail")
		assert.True(t, hasConstraint(errs, "type"))
	})
}

func Test_Validate_NumericStringType(t *testing.T) {
	schemaJSON := []byte(`{
		"type": "object",
		"properties": {
			"count": {"type": "number"}
		}
	}`)

	// String "42" should NOT be coerced to a number — should fail.
	errs, err := schema.Validate(schemaJSON, map[string]any{"count": "42"})
	require.NoError(t, err)
	require.NotEmpty(t, errs, "string '42' should fail number type check (no implicit coercion)")
	assert.True(t, hasConstraint(errs, "type"), "constraint should be 'type'")
}

// ===========================================================================
// Phase 12: Integration with Common Schema
// ===========================================================================

func Test_Validate_CommonSchema(t *testing.T) {
	event := validBaseEvent()

	errs, err := schema.ValidateCommonSchema(event)
	require.NoError(t, err)
	assert.Empty(t, errs, "properly formed event should pass common schema validation")
}

func Test_Validate_CommonSchema_MissingMessageId(t *testing.T) {
	event := validBaseEvent()
	delete(event, "messageId")

	errs, err := schema.ValidateCommonSchema(event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "event without messageId should fail common schema validation")
	assert.True(t, hasMessageContaining(errs, "messageId"),
		"error should reference missing messageId")
}

func Test_Validate_CommonSchema_MissingType(t *testing.T) {
	event := validBaseEvent()
	delete(event, "type")

	errs, err := schema.ValidateCommonSchema(event)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "event without type should fail common schema validation")
	assert.True(t, hasMessageContaining(errs, "type"),
		"error should reference missing type")
}
