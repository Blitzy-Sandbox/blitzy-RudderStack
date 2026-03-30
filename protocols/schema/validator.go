// Package schema provides a high-performance, local JSON Schema draft-07
// validation engine for Protocols enforcement (E-020, Sprint 5-7).
//
// It replaces the network-based Transformer delegation in
// processor/trackingplan.go with direct validation using the
// santhosh-tekuri/jsonschema/v5 library. Pre-compiled schemas enable efficient
// repeated validation of event batches flowing through the RudderStack pipeline.
//
// Key types and functions:
//
//   - CompileSchema: pre-compiles a JSON Schema draft-07 definition.
//   - Validate / ValidateWithCompiled: validate a single event.
//   - ValidateBatch / ValidateBatchWithCompiled: validate a batch of events.
//   - ToProcessorValidationErrors: bridge to processor/types.ValidationError shape.
package schema

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/rudderlabs/rudder-go-kit/logger"
)

// pkgLogger is the package-level structured logger, following the
// warehouse/identity/identity.go pattern of hierarchical child loggers.
// It is used by both validator.go and common_schema.go within this package.
var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("protocols").Child("schema")
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// ValidationError represents a single JSON Schema validation failure.
// It provides detailed information about what constraint was violated and where,
// enabling downstream enforcement logic (processor/enforcement/modes.go) to
// make granular decisions based on constraint type.
type ValidationError struct {
	// FieldPath is the dot-notation JSON path to the field that failed validation
	// (e.g., "properties.address.city"). Converted from the JSON Pointer notation
	// used internally by the jsonschema library.
	FieldPath string `json:"fieldPath"`

	// ExpectedType is the type expected by the schema (e.g., "string", "integer",
	// "object"). Only populated for type-constraint violations; empty for other
	// constraint types.
	ExpectedType string `json:"expectedType"`

	// ActualValue is a string representation of the actual value that failed
	// validation. Large values are truncated for readability and security.
	ActualValue string `json:"actualValue"`

	// Constraint is the name of the JSON Schema keyword that was violated
	// (e.g., "required", "type", "pattern", "enum", "format", "minimum",
	// "maximum", "minLength", "maxLength", "minItems", "maxItems",
	// "additionalProperties").
	Constraint string `json:"constraint"`

	// Message is a human-readable description of the validation error produced
	// by the jsonschema library.
	Message string `json:"message"`
}

// CompiledSchema represents a pre-compiled JSON Schema that can be reused
// across multiple validation calls. Compiling a schema is expensive (parsing,
// reference resolution), so pre-compilation amortizes that cost when the same
// schema is applied to many events.
type CompiledSchema struct {
	schema *jsonschema.Schema
}

// ---------------------------------------------------------------------------
// Compile
// ---------------------------------------------------------------------------

// CompileSchema pre-compiles a JSON Schema draft-07 definition for reuse across
// multiple validation calls. Returns a CompiledSchema that can be passed to
// ValidateWithCompiled and ValidateBatchWithCompiled.
//
// The schema bytes must be valid JSON containing a JSON Schema draft-07
// definition. Returns an error if the schema is invalid JSON or violates the
// JSON Schema meta-schema.
func CompileSchema(schema []byte) (*CompiledSchema, error) {
	if schema == nil {
		return nil, fmt.Errorf("schema cannot be nil")
	}

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft7

	// AddResource expects an io.Reader, so wrap the raw bytes directly.
	// This avoids unmarshaling/re-marshaling and is the idiomatic v5 API.
	const schemaURI = "schema.json"
	if err := compiler.AddResource(schemaURI, bytes.NewReader(schema)); err != nil {
		pkgLogger.Debugn("failed to add schema resource", logger.NewStringField("error", err.Error()))
		return nil, fmt.Errorf("invalid JSON Schema: %w", err)
	}

	compiled, err := compiler.Compile(schemaURI)
	if err != nil {
		pkgLogger.Debugn("schema compilation failed", logger.NewStringField("error", err.Error()))
		return nil, fmt.Errorf("schema compilation failed: %w", err)
	}

	return &CompiledSchema{schema: compiled}, nil
}

// ---------------------------------------------------------------------------
// Single-event validation
// ---------------------------------------------------------------------------

// Validate validates a single event against a JSON Schema draft-07 definition.
// The schema is compiled on each call. For repeated validations against the
// same schema, use CompileSchema + ValidateWithCompiled instead.
//
// Returns a slice of ValidationError for each constraint violation, and an
// error only if the schema itself is invalid or an internal error occurs.
func Validate(schema []byte, event map[string]any) ([]ValidationError, error) {
	compiled, err := CompileSchema(schema)
	if err != nil {
		return nil, fmt.Errorf("compiling schema: %w", err)
	}
	return ValidateWithCompiled(compiled, event)
}

// ValidateWithCompiled validates a single event against a pre-compiled JSON
// Schema. This is the preferred method for repeated validations against the
// same schema.
//
// Returns a slice of ValidationError for each constraint violation. An
// empty/nil slice indicates the event is valid. Returns an error only for
// internal errors (not validation failures).
func ValidateWithCompiled(compiled *CompiledSchema, event map[string]any) ([]ValidationError, error) {
	if compiled == nil {
		return nil, fmt.Errorf("compiled schema cannot be nil")
	}
	if event == nil {
		return nil, fmt.Errorf("event cannot be nil")
	}

	err := compiled.schema.Validate(event)
	if err == nil {
		// Event passes all schema constraints.
		return nil, nil
	}

	// The jsonschema library returns *jsonschema.ValidationError on
	// validation failures and other error types for internal issues.
	validationErr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return nil, fmt.Errorf("unexpected validation error type: %w", err)
	}

	return extractValidationErrors(validationErr), nil
}

// ---------------------------------------------------------------------------
// Batch validation
// ---------------------------------------------------------------------------

// ValidateBatch validates a batch of events against a JSON Schema draft-07
// definition. The schema is compiled once and reused for all events in the
// batch. Returns a slice of validation error slices — one per event — and an
// error if the schema itself is invalid.
func ValidateBatch(schema []byte, events []map[string]any) ([][]ValidationError, error) {
	compiled, err := CompileSchema(schema)
	if err != nil {
		return nil, fmt.Errorf("compiling schema: %w", err)
	}
	return ValidateBatchWithCompiled(compiled, events)
}

// ValidateBatchWithCompiled validates a batch of events against a pre-compiled
// JSON Schema. Returns a slice of validation error slices — one per event.
// Each event's errors are independent. An empty/nil inner slice means that
// event is valid.
func ValidateBatchWithCompiled(compiled *CompiledSchema, events []map[string]any) ([][]ValidationError, error) {
	if compiled == nil {
		return nil, fmt.Errorf("compiled schema cannot be nil")
	}

	results := make([][]ValidationError, len(events))
	for i, event := range events {
		errs, err := ValidateWithCompiled(compiled, event)
		if err != nil {
			return nil, fmt.Errorf("validating event %d: %w", i, err)
		}
		results[i] = errs
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// Processor bridge
// ---------------------------------------------------------------------------

// ToProcessorValidationErrors converts schema ValidationErrors to the format
// expected by the processor pipeline (processor/types.ValidationError).
// This bridge enables the protocols/schema package to integrate with the
// existing processor tracking plan validation flow without introducing a
// circular dependency on the processor/types package.
//
// The returned slice of maps matches the JSON shape of
// processor/types.ValidationError:
//
//	{
//	  "type":     <constraint>,
//	  "message":  <message>,
//	  "property": <fieldPath>,
//	  "meta":     {"expectedType": <...>, "actualValue": <...>}
//	}
func ToProcessorValidationErrors(errs []ValidationError) []map[string]any {
	if errs == nil {
		return nil
	}
	result := make([]map[string]any, len(errs))
	for i, e := range errs {
		result[i] = map[string]any{
			"type":     e.Constraint,
			"message":  e.Message,
			"property": e.FieldPath,
			"meta": map[string]string{
				"expectedType": e.ExpectedType,
				"actualValue":  e.ActualValue,
			},
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Internal helpers — error extraction
// ---------------------------------------------------------------------------

// extractValidationErrors recursively extracts validation errors from the
// jsonschema.ValidationError tree structure into a flat slice of
// ValidationError. The jsonschema library returns a tree where internal nodes
// aggregate child failures and leaf nodes carry the actual constraint
// violation details.
func extractValidationErrors(ve *jsonschema.ValidationError) []ValidationError {
	if ve == nil {
		return nil
	}

	var errors []ValidationError

	// Leaf error — this is an actual constraint violation.
	if len(ve.Causes) == 0 {
		verr := ValidationError{
			FieldPath:    extractFieldPath(ve),
			Constraint:   extractConstraint(ve),
			Message:      ve.Message,
			ExpectedType: extractExpectedType(ve),
			ActualValue:  extractActualValue(ve),
		}
		errors = append(errors, verr)
	}

	// Recursively process child errors.
	for _, cause := range ve.Causes {
		errors = append(errors, extractValidationErrors(cause)...)
	}

	return errors
}

// extractFieldPath converts the JSON Pointer in InstanceLocation to
// dot-notation for consistency with RudderStack conventions.
//
// Example: "/properties/address/city" → "properties.address.city"
func extractFieldPath(ve *jsonschema.ValidationError) string {
	loc := ve.InstanceLocation
	if loc == "" {
		return ""
	}
	// Remove the leading '/' and replace remaining '/' with '.'
	path := strings.TrimPrefix(loc, "/")
	return strings.ReplaceAll(path, "/", ".")
}

// extractConstraint determines which JSON Schema constraint was violated by
// inspecting the KeywordLocation. The last path segment identifies the
// keyword (e.g., "type", "required", "pattern").
func extractConstraint(ve *jsonschema.ValidationError) string {
	loc := ve.KeywordLocation
	if loc == "" {
		return "unknown"
	}
	// Extract the last segment of the slash-separated keyword path.
	parts := strings.Split(strings.TrimPrefix(loc, "/"), "/")
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if last != "" {
			return last
		}
	}
	return "unknown"
}

// extractExpectedType returns the expected type string for type-constraint
// violations. For other constraint types, it returns an empty string.
func extractExpectedType(ve *jsonschema.ValidationError) string {
	if !strings.Contains(ve.KeywordLocation, "/type") {
		return ""
	}
	// jsonschema v5 type error messages follow the pattern:
	//   "expected <type>, but got <actualType>"
	return extractTypeFromMessage(ve.Message)
}

// extractActualValue returns a string representation of the actual value that
// caused the validation failure. The value is derived from the error message
// to avoid retaining potentially large event payloads.
// Large values are truncated for readability and security.
func extractActualValue(ve *jsonschema.ValidationError) string {
	val := fmt.Sprintf("%v", ve.Message)
	const maxLen = 200
	if len(val) > maxLen {
		return val[:maxLen] + "..."
	}
	return val
}

// extractTypeFromMessage parses the expected type from a jsonschema type-error
// message. Messages follow the pattern "expected <type>, but got <actualType>".
func extractTypeFromMessage(message string) string {
	if strings.HasPrefix(message, "expected ") {
		parts := strings.SplitN(message, ",", 2)
		if len(parts) > 0 {
			return strings.TrimPrefix(parts[0], "expected ")
		}
	}
	return ""
}
