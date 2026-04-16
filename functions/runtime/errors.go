// Package runtime provides the Functions runtime engine for the RudderStack
// pipeline, supporting Source Functions, Destination Functions, and Insert
// Functions (Epics E-015, E-016, E-017).
//
// This file defines typed error classes that the Functions runtime uses to
// communicate error semantics to downstream consumers. Each error type carries
// retry behaviour information so that the pipeline can decide whether to retry,
// drop, or propagate a failure without inspecting error strings.
package runtime

import (
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Error type string constants
// ---------------------------------------------------------------------------
// These constants match the error type names returned by the Transformer
// service. They are used by engine.go's mapFunctionError to map Transformer
// response error codes to the corresponding Go typed errors defined below.
const (
	// ErrorTypeEventNotSupported identifies an error where the handler for
	// the requested event type is not implemented in the user's function code.
	ErrorTypeEventNotSupported = "EventNotSupported"

	// ErrorTypeInvalidEventPayload identifies an error where the event data
	// is malformed or missing required fields.
	ErrorTypeInvalidEventPayload = "InvalidEventPayload"

	// ErrorTypeValidationError identifies an error where the event fails
	// validation rules defined in the function.
	ErrorTypeValidationError = "ValidationError"

	// ErrorTypeRetryError identifies a transient failure that should be
	// retried with backoff.
	ErrorTypeRetryError = "RetryError"

	// ErrorTypeDropEvent identifies an intentional event drop by the
	// function logic (not an error condition per se).
	ErrorTypeDropEvent = "DropEvent"
)

// ---------------------------------------------------------------------------
// Retryable interface
// ---------------------------------------------------------------------------

// Retryable is an interface that function errors can implement to indicate
// whether the error is transient and the operation should be retried.
// All five error types in this file implement Retryable so that callers can
// use a single type-assertion to determine retry behaviour.
type Retryable interface {
	IsRetryable() bool
}

// ---------------------------------------------------------------------------
// Error type: EventNotSupported
// ---------------------------------------------------------------------------

// EventNotSupported is returned when a handler for the event type is not
// implemented in the user's function code. For example, a Source Function that
// does not handle GET requests, or a Destination Function with no onAlias
// handler.
//
// This is a non-retryable error — the event type will never be supported by
// the function regardless of how many times the operation is retried.
type EventNotSupported struct {
	// Message describes which event type or handler is not supported.
	Message string
}

// Error implements the error interface for EventNotSupported.
func (e *EventNotSupported) Error() string {
	return fmt.Sprintf("event not supported: %s", e.Message)
}

// IsRetryable implements the Retryable interface. EventNotSupported errors are
// never retryable because the function will never support the event type.
func (e *EventNotSupported) IsRetryable() bool {
	return false
}

// ---------------------------------------------------------------------------
// Error type: InvalidEventPayload
// ---------------------------------------------------------------------------

// InvalidEventPayload is returned when the event data is malformed or missing
// required fields. For example, a missing "type" field or an unparseable JSON
// body.
//
// This is a non-retryable error — the payload will never become valid by
// retrying.
type InvalidEventPayload struct {
	// Message describes what is wrong with the payload.
	Message string
}

// Error implements the error interface for InvalidEventPayload.
func (e *InvalidEventPayload) Error() string {
	return fmt.Sprintf("invalid event payload: %s", e.Message)
}

// IsRetryable implements the Retryable interface. InvalidEventPayload errors
// are never retryable because the payload itself is defective.
func (e *InvalidEventPayload) IsRetryable() bool {
	return false
}

// ---------------------------------------------------------------------------
// Error type: ValidationError
// ---------------------------------------------------------------------------

// ValidationError is returned when the event fails validation rules defined in
// the function. For example, a required field is missing or a field value is
// out of an acceptable range.
//
// This is a non-retryable error — the event does not conform to the expected
// schema.
type ValidationError struct {
	// Message describes which validation rule was violated.
	Message string
}

// Error implements the error interface for ValidationError.
func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error: %s", e.Message)
}

// IsRetryable implements the Retryable interface. ValidationError errors are
// never retryable because the event will not suddenly pass validation on retry.
func (e *ValidationError) IsRetryable() bool {
	return false
}

// ---------------------------------------------------------------------------
// Error type: RetryError
// ---------------------------------------------------------------------------

// RetryError is returned for transient failures that should be retried with
// backoff. For example, a temporary network failure when calling an external
// API, rate limiting from a downstream service, or a request timeout.
//
// This is the ONLY retryable error type — the operation may succeed on retry.
type RetryError struct {
	// Message describes the transient failure.
	Message string
}

// Error implements the error interface for RetryError.
func (e *RetryError) Error() string {
	return fmt.Sprintf("retry error: %s", e.Message)
}

// IsRetryable implements the Retryable interface. RetryError is the only error
// type that returns true, signalling that the caller should retry the operation
// with appropriate backoff.
func (e *RetryError) IsRetryable() bool {
	return true
}

// ---------------------------------------------------------------------------
// Error type: DropEvent
// ---------------------------------------------------------------------------

// DropEvent is returned when a function intentionally drops an event. This is
// not an error condition in the traditional sense — it signals that the
// function's business logic has decided the event should be discarded. For
// example, an Insert Function filtering out internal test events or a
// Destination Function skipping events from specific sources.
//
// This is a non-retryable error — the event was intentionally discarded.
type DropEvent struct {
	// Message describes the reason the event was dropped.
	Message string
}

// Error implements the error interface for DropEvent.
func (e *DropEvent) Error() string {
	return fmt.Sprintf("event dropped: %s", e.Message)
}

// IsRetryable implements the Retryable interface. DropEvent errors are never
// retryable because the drop was an intentional decision by the function logic.
func (e *DropEvent) IsRetryable() bool {
	return false
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// IsRetryableError checks whether the given error is retryable.
// It returns true only if the error (or any error in its chain) implements the
// Retryable interface and IsRetryable() returns true.
//
// Usage:
//
//	if runtime.IsRetryableError(err) {
//	    // schedule retry with backoff
//	}
func IsRetryableError(err error) bool {
	var r Retryable
	if errors.As(err, &r) {
		return r.IsRetryable()
	}
	return false
}

// IsDropError checks whether the given error indicates the event was
// intentionally dropped by the function logic. It unwraps the error chain
// using errors.As to handle wrapped errors correctly.
//
// Usage:
//
//	if runtime.IsDropError(err) {
//	    // acknowledge event as processed without sending downstream
//	}
func IsDropError(err error) bool {
	var d *DropEvent
	return errors.As(err, &d)
}
