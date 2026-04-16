package runtime

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// ==========================================================================
// Phase 1: Error Type Instantiation Tests
// ==========================================================================

// TestEventNotSupported_Error verifies that EventNotSupported implements the
// error interface and its Error() method returns a string containing the
// provided message.
func TestEventNotSupported_Error(t *testing.T) {
	err := &EventNotSupported{Message: "handler not implemented for screen"}
	require.NotNil(t, err)
	require.Contains(t, err.Error(), "handler not implemented for screen")
	require.Equal(t, "event not supported: handler not implemented for screen", err.Error())
}

// TestInvalidEventPayload_Error verifies that InvalidEventPayload implements
// the error interface and its Error() method returns a string containing the
// provided message.
func TestInvalidEventPayload_Error(t *testing.T) {
	err := &InvalidEventPayload{Message: "missing required field 'type'"}
	require.NotNil(t, err)
	require.Contains(t, err.Error(), "missing required field 'type'")
	require.Equal(t, "invalid event payload: missing required field 'type'", err.Error())
}

// TestValidationError_Error verifies that ValidationError implements the error
// interface and its Error() method returns a string containing the provided
// message.
func TestValidationError_Error(t *testing.T) {
	err := &ValidationError{Message: "event failed schema validation"}
	require.NotNil(t, err)
	require.Contains(t, err.Error(), "event failed schema validation")
	require.Equal(t, "validation error: event failed schema validation", err.Error())
}

// TestRetryError_Error verifies that RetryError implements the error interface
// and its Error() method returns a string containing the provided message.
func TestRetryError_Error(t *testing.T) {
	err := &RetryError{Message: "temporary network failure"}
	require.NotNil(t, err)
	require.Contains(t, err.Error(), "temporary network failure")
	require.Equal(t, "retry error: temporary network failure", err.Error())
}

// TestDropEvent_Error verifies that DropEvent implements the error interface
// and its Error() method returns a string containing the provided message.
func TestDropEvent_Error(t *testing.T) {
	err := &DropEvent{Message: "event intentionally dropped by function logic"}
	require.NotNil(t, err)
	require.Contains(t, err.Error(), "event intentionally dropped by function logic")
	require.Equal(t, "event dropped: event intentionally dropped by function logic", err.Error())
}

// TestErrorTypes_EmptyMessage verifies that each error type handles an empty
// Message field gracefully, producing a valid error string without panicking.
func TestErrorTypes_EmptyMessage(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{"EventNotSupported/empty", &EventNotSupported{Message: ""}, "event not supported: "},
		{"InvalidEventPayload/empty", &InvalidEventPayload{Message: ""}, "invalid event payload: "},
		{"ValidationError/empty", &ValidationError{Message: ""}, "validation error: "},
		{"RetryError/empty", &RetryError{Message: ""}, "retry error: "},
		{"DropEvent/empty", &DropEvent{Message: ""}, "event dropped: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, tc.err.Error())
		})
	}
}

// ==========================================================================
// Phase 2: IsRetryable Behavior Tests
// ==========================================================================

// TestEventNotSupported_IsRetryable verifies that EventNotSupported is
// non-retryable — the event type will never be supported by the function.
func TestEventNotSupported_IsRetryable(t *testing.T) {
	err := &EventNotSupported{Message: "no handler for screen"}
	require.False(t, err.IsRetryable())
}

// TestInvalidEventPayload_IsRetryable verifies that InvalidEventPayload is
// non-retryable — the payload will never become valid by retrying.
func TestInvalidEventPayload_IsRetryable(t *testing.T) {
	err := &InvalidEventPayload{Message: "missing type field"}
	require.False(t, err.IsRetryable())
}

// TestValidationError_IsRetryable verifies that ValidationError is
// non-retryable — the event does not conform to the expected schema.
func TestValidationError_IsRetryable(t *testing.T) {
	err := &ValidationError{Message: "required field missing"}
	require.False(t, err.IsRetryable())
}

// TestRetryError_IsRetryable verifies that RetryError IS retryable — the
// operation may succeed on retry. This is the ONLY retryable error type.
func TestRetryError_IsRetryable(t *testing.T) {
	err := &RetryError{Message: "temporary network failure"}
	require.True(t, err.IsRetryable())
}

// TestDropEvent_IsRetryable verifies that DropEvent is non-retryable — the
// event was intentionally discarded by the function logic.
func TestDropEvent_IsRetryable(t *testing.T) {
	err := &DropEvent{Message: "intentionally dropped"}
	require.False(t, err.IsRetryable())
}

// TestIsRetryable_ViaInterface verifies that all five error types implement
// the Retryable interface and that the IsRetryable behaviour is accessible
// via the interface type assertion.
func TestIsRetryable_ViaInterface(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"EventNotSupported", &EventNotSupported{Message: "x"}, false},
		{"InvalidEventPayload", &InvalidEventPayload{Message: "x"}, false},
		{"ValidationError", &ValidationError{Message: "x"}, false},
		{"RetryError", &RetryError{Message: "x"}, true},
		{"DropEvent", &DropEvent{Message: "x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := tc.err.(Retryable)
			require.True(t, ok, "error should implement Retryable interface")
			require.Equal(t, tc.expected, r.IsRetryable())
		})
	}
}

// ==========================================================================
// Phase 3: Error Interface Compliance Tests
// ==========================================================================

// TestAllErrorTypes_SatisfyErrorInterface verifies that all five error types
// satisfy the built-in error interface by creating a []error slice and
// iterating over each element.
func TestAllErrorTypes_SatisfyErrorInterface(t *testing.T) {
	errs := []error{
		&EventNotSupported{Message: "screen not supported"},
		&InvalidEventPayload{Message: "missing type"},
		&ValidationError{Message: "schema mismatch"},
		&RetryError{Message: "transient failure"},
		&DropEvent{Message: "dropped by logic"},
	}

	require.Equal(t, 5, len(errs), "all 5 error types must be present")

	for i, err := range errs {
		require.NotNil(t, err, "error at index %d should not be nil", i)
		require.NotEmpty(t, err.Error(), "error at index %d should have non-empty Error() string", i)
	}
}

// TestEventNotSupported_ErrorsAs verifies that an error variable typed as
// `error` holding an *EventNotSupported value can be unwrapped via errors.As.
func TestEventNotSupported_ErrorsAs(t *testing.T) {
	var err error = &EventNotSupported{Message: "test errors.As"}
	var target *EventNotSupported
	require.True(t, errors.As(err, &target))
	require.NotNil(t, target)
	require.Equal(t, "test errors.As", target.Message)
}

// TestInvalidEventPayload_ErrorsAs verifies errors.As unwrapping for
// *InvalidEventPayload.
func TestInvalidEventPayload_ErrorsAs(t *testing.T) {
	var err error = &InvalidEventPayload{Message: "test errors.As"}
	var target *InvalidEventPayload
	require.True(t, errors.As(err, &target))
	require.NotNil(t, target)
	require.Equal(t, "test errors.As", target.Message)
}

// TestValidationError_ErrorsAs verifies errors.As unwrapping for
// *ValidationError.
func TestValidationError_ErrorsAs(t *testing.T) {
	var err error = &ValidationError{Message: "test errors.As"}
	var target *ValidationError
	require.True(t, errors.As(err, &target))
	require.NotNil(t, target)
	require.Equal(t, "test errors.As", target.Message)
}

// TestRetryError_ErrorsAs verifies errors.As unwrapping for *RetryError.
func TestRetryError_ErrorsAs(t *testing.T) {
	var err error = &RetryError{Message: "test errors.As"}
	var target *RetryError
	require.True(t, errors.As(err, &target))
	require.NotNil(t, target)
	require.Equal(t, "test errors.As", target.Message)
}

// TestDropEvent_ErrorsAs verifies errors.As unwrapping for *DropEvent.
func TestDropEvent_ErrorsAs(t *testing.T) {
	var err error = &DropEvent{Message: "test errors.As"}
	var target *DropEvent
	require.True(t, errors.As(err, &target))
	require.NotNil(t, target)
	require.Equal(t, "test errors.As", target.Message)
}

// TestErrorsAs_CrossTypeNegative verifies that errors.As correctly returns
// false when trying to unwrap an error to an incompatible type. For example,
// an *EventNotSupported should NOT match *RetryError.
func TestErrorsAs_CrossTypeNegative(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"EventNotSupported", &EventNotSupported{Message: "x"}},
		{"InvalidEventPayload", &InvalidEventPayload{Message: "x"}},
		{"ValidationError", &ValidationError{Message: "x"}},
		{"RetryError", &RetryError{Message: "x"}},
		{"DropEvent", &DropEvent{Message: "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name+"_not_DropEvent", func(t *testing.T) {
			if _, ok := tc.err.(*DropEvent); ok {
				t.Skip("skipping DropEvent for DropEvent target")
			}
			var target *DropEvent
			// errors.As should return false for non-matching types
			if errors.As(tc.err, &target) {
				t.Errorf("expected errors.As to return false for %T → *DropEvent", tc.err)
			}
		})
		t.Run(tc.name+"_not_RetryError", func(t *testing.T) {
			if _, ok := tc.err.(*RetryError); ok {
				t.Skip("skipping RetryError for RetryError target")
			}
			var target *RetryError
			if errors.As(tc.err, &target) {
				t.Errorf("expected errors.As to return false for %T → *RetryError", tc.err)
			}
		})
	}
}

// ==========================================================================
// Phase 4: IsDropError and IsRetryableError Helper Tests
// ==========================================================================

// TestIsDropError verifies the IsDropError helper function:
//   - Returns true for *DropEvent
//   - Returns false for all other function error types
//   - Returns false for a generic error created via fmt.Errorf
func TestIsDropError(t *testing.T) {
	t.Run("DropEvent_returns_true", func(t *testing.T) {
		err := &DropEvent{Message: "intentionally dropped"}
		require.True(t, IsDropError(err))
	})
	t.Run("RetryError_returns_false", func(t *testing.T) {
		err := &RetryError{Message: "transient"}
		require.False(t, IsDropError(err))
	})
	t.Run("EventNotSupported_returns_false", func(t *testing.T) {
		err := &EventNotSupported{Message: "unsupported"}
		require.False(t, IsDropError(err))
	})
	t.Run("InvalidEventPayload_returns_false", func(t *testing.T) {
		err := &InvalidEventPayload{Message: "bad payload"}
		require.False(t, IsDropError(err))
	})
	t.Run("ValidationError_returns_false", func(t *testing.T) {
		err := &ValidationError{Message: "invalid"}
		require.False(t, IsDropError(err))
	})
	t.Run("GenericError_returns_false", func(t *testing.T) {
		err := fmt.Errorf("some error")
		require.False(t, IsDropError(err))
	})
	t.Run("NilError_returns_false", func(t *testing.T) {
		require.False(t, IsDropError(nil))
	})
}

// TestIsRetryableError verifies the IsRetryableError helper function:
//   - Returns true ONLY for *RetryError
//   - Returns false for all other function error types
//   - Returns false for a generic error created via fmt.Errorf
//   - Returns false for nil errors
func TestIsRetryableError(t *testing.T) {
	t.Run("RetryError_returns_true", func(t *testing.T) {
		err := &RetryError{Message: "temporary failure"}
		require.True(t, IsRetryableError(err))
	})
	t.Run("EventNotSupported_returns_false", func(t *testing.T) {
		err := &EventNotSupported{Message: "unsupported"}
		require.False(t, IsRetryableError(err))
	})
	t.Run("InvalidEventPayload_returns_false", func(t *testing.T) {
		err := &InvalidEventPayload{Message: "bad payload"}
		require.False(t, IsRetryableError(err))
	})
	t.Run("ValidationError_returns_false", func(t *testing.T) {
		err := &ValidationError{Message: "invalid"}
		require.False(t, IsRetryableError(err))
	})
	t.Run("DropEvent_returns_false", func(t *testing.T) {
		err := &DropEvent{Message: "dropped"}
		require.False(t, IsRetryableError(err))
	})
	t.Run("GenericError_returns_false", func(t *testing.T) {
		err := fmt.Errorf("some error")
		require.False(t, IsRetryableError(err))
	})
	t.Run("NilError_returns_false", func(t *testing.T) {
		require.False(t, IsRetryableError(nil))
	})
}

// TestIsDropError_WrappedError verifies that IsDropError correctly detects
// a *DropEvent even when it is wrapped via fmt.Errorf with %w.
func TestIsDropError_WrappedError(t *testing.T) {
	inner := &DropEvent{Message: "dropped inside"}
	wrapped := fmt.Errorf("outer context: %w", inner)
	require.True(t, IsDropError(wrapped))
}

// TestIsRetryableError_WrappedError verifies that IsRetryableError correctly
// detects a *RetryError even when it is wrapped via fmt.Errorf with %w.
func TestIsRetryableError_WrappedError(t *testing.T) {
	inner := &RetryError{Message: "transient inside"}
	wrapped := fmt.Errorf("outer context: %w", inner)
	require.True(t, IsRetryableError(wrapped))
}

// TestIsRetryableError_WrappedNonRetryable verifies that IsRetryableError
// returns false when a non-retryable error is wrapped.
func TestIsRetryableError_WrappedNonRetryable(t *testing.T) {
	inner := &EventNotSupported{Message: "unsupported inside"}
	wrapped := fmt.Errorf("outer: %w", inner)
	require.False(t, IsRetryableError(wrapped))
}

// ==========================================================================
// Phase 5: Error Type String Constants Tests
// ==========================================================================

// TestErrorTypeConstants verifies that the error type string constants are
// defined with the expected values, matching the Transformer service error
// type names.
func TestErrorTypeConstants(t *testing.T) {
	require.Equal(t, "EventNotSupported", ErrorTypeEventNotSupported)
	require.Equal(t, "InvalidEventPayload", ErrorTypeInvalidEventPayload)
	require.Equal(t, "ValidationError", ErrorTypeValidationError)
	require.Equal(t, "RetryError", ErrorTypeRetryError)
	require.Equal(t, "DropEvent", ErrorTypeDropEvent)
}
