// Package graph — processor_adapter.go provides an adapter that bridges the identity
// graph's Service interface with the processor's identityResolver interface (Gap 6, E-026).
//
// The processor expects:
//
//	type identityResolver interface {
//	    ResolveIdentity(ctx context.Context, event types.TransformerEvent) error
//	}
//
// The graph.Service provides:
//
//	ProcessEvent(ctx context.Context, workspaceID string, eventJSON []byte) (*ResolutionResult, error)
//
// This adapter marshals the TransformerEvent.Message to JSON bytes and calls ProcessEvent.
package graph

import (
	"context"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/processor/types"
)

// ProcessorAdapter adapts graph.Service to the processor's identityResolver interface.
// It marshals each TransformerEvent's Message (map[string]any) to JSON bytes and
// delegates to graph.Service.ProcessEvent for real-time identity resolution.
//
// Thread-safe: ProcessEvent is safe for concurrent use.
type ProcessorAdapter struct {
	svc Service
	log logger.Logger
}

// NewProcessorAdapter creates an adapter bridging graph.Service to the processor's
// identityResolver interface. The returned adapter satisfies:
//
//	type identityResolver interface {
//	    ResolveIdentity(ctx context.Context, event types.TransformerEvent) error
//	}
//
// Parameters:
//   - svc: the identity graph service that processes events
//   - log: structured logger for error reporting
func NewProcessorAdapter(svc Service, log logger.Logger) *ProcessorAdapter {
	return &ProcessorAdapter{
		svc: svc,
		log: log,
	}
}

// ResolveIdentity marshals the TransformerEvent's Message to JSON and delegates
// to graph.Service.ProcessEvent for real-time identity resolution.
//
// The workspaceID is extracted from the event's Metadata.WorkspaceID field.
// Returns nil when the event produces no identifiers (empty resolution is normal).
func (a *ProcessorAdapter) ResolveIdentity(ctx context.Context, event types.TransformerEvent) error {
	eventJSON, err := jsonrs.Marshal(event.Message)
	if err != nil {
		a.log.Warnn("identity resolver: failed to marshal event message",
			logger.NewStringField("error", err.Error()),
			logger.NewStringField("sourceID", event.Metadata.SourceID),
		)
		return err
	}

	workspaceID := event.Metadata.WorkspaceID
	if workspaceID == "" {
		// Defensive: if workspaceID is empty, skip resolution rather than error.
		return nil
	}

	_, processErr := a.svc.ProcessEvent(ctx, workspaceID, eventJSON)
	if processErr != nil {
		return processErr
	}

	return nil
}
