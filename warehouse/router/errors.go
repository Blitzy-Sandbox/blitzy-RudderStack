package router

import (
	"strings"

	"github.com/rudderlabs/rudder-server/warehouse/internal/model"
)

const (
	// BackfillError indicates a failure specifically during a backfill operation.
	BackfillError = "backfill_error"

	// ReplayError indicates a failure during a warehouse replay operation.
	ReplayError = "replay_error"

	// HealthCheckError indicates a failure in health monitoring operations.
	HealthCheckError = "health_check_error"
)

type errorMapper interface {
	ErrorMappings() []model.JobError
}

type ErrorHandler struct {
	Mapper errorMapper
}

// MatchUploadJobErrorType matches the error with the error mappings defined in the integrations
// and returns the corresponding matched error type else returns UncategorizedError
func (e *ErrorHandler) MatchUploadJobErrorType(err error) model.JobErrorType {
	if e.Mapper == nil || err == nil {
		return model.UncategorizedError
	}

	errString := err.Error()

	// Check for backfill-specific errors before consulting the mapper.
	if strings.Contains(errString, "backfill") {
		return BackfillError
	}
	// Check for replay-specific errors before consulting the mapper.
	if strings.Contains(errString, "replay") {
		return ReplayError
	}

	for _, em := range e.Mapper.ErrorMappings() {
		if em.Format.MatchString(errString) {
			return em.Type
		}
	}

	return model.UncategorizedError
}

// GetErrorCategory returns the error category string for a given error.
// Used by the health monitor to categorize sync failures.
func GetErrorCategory(handler *ErrorHandler, err error) string {
	if err == nil {
		return ""
	}
	return handler.MatchUploadJobErrorType(err)
}
