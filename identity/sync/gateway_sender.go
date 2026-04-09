// Package sync — gateway_sender.go implements a DestinationSender that delivers
// profile updates to downstream destinations by writing identify events into the
// gateway DB (Gap 7, E-029). The events then flow through the normal processor →
// router pipeline, leveraging existing destination authentication, batching, retry,
// and delivery infrastructure.
//
// This replaces the log-only LogDestinationSender with an implementation that
// actually delivers profile changes to configured downstream destinations.
package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/identity/storage"
)

// ProfileEventWriter is a callback that writes profile sync events into the pipeline.
// The embeddedAppHandler provides a concrete implementation that calls
// jobsdb.WithStoreSafeTx to store events in the gateway DB.
type ProfileEventWriter func(ctx context.Context, workspaceID string, events []json.RawMessage) error

// GatewayDestinationSender implements DestinationSender by converting profile data
// into identify events and writing them to the gateway DB. The events then flow
// through the standard processor → router pipeline for delivery to all destinations
// configured for the profile's workspace.
//
// Architecture:
//
//	Profile change → GatewayDestinationSender.SendProfile → Gateway DB →
//	Processor (identity, transforms) → Router → Destinations
//
// Thread-safe: all methods can be called concurrently.
type GatewayDestinationSender struct {
	writer       ProfileEventWriter
	log          logger.Logger
	statsFactory stats.Stats
}

// NewGatewayDestinationSender creates a DestinationSender that delivers profile
// updates by writing identify events into the gateway DB.
//
// Parameters:
//   - writer: callback that writes events into the gateway DB pipeline
//   - log: structured logger for delivery logging
//   - statsFactory: stats factory for delivery metrics
func NewGatewayDestinationSender(writer ProfileEventWriter, log logger.Logger, statsFactory stats.Stats) *GatewayDestinationSender {
	if log == nil {
		log = pkgLogger
	}
	return &GatewayDestinationSender{
		writer:       writer,
		log:          log.Child("gateway-sender"),
		statsFactory: statsFactory,
	}
}

// SendProfile converts a profile update into an identify event and writes it to
// the gateway DB for delivery through the standard pipeline.
//
// The generated identify event includes:
//   - type: "identify"
//   - userId: primary user ID from external identifiers
//   - traits: all traits from the profile
//   - context.externalIds: all external identifiers
//   - context.profileSync: true (marks this as a profile sync event)
//   - context.segmentId: the identity graph segment ID
func (s *GatewayDestinationSender) SendProfile(ctx context.Context, profile *storage.ProfileData) error {
	if profile == nil {
		return fmt.Errorf("identity sync gateway sender: nil profile")
	}

	if s.writer == nil {
		s.log.Warnn("No gateway event writer configured; profile sync event not delivered",
			logger.NewIntField("segmentID", profile.Segment.ID),
		)
		return nil
	}

	event := s.buildIdentifyEvent(profile)
	eventJSON, err := jsonrs.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal profile sync event: %w", err)
	}

	if writeErr := s.writer(ctx, profile.Segment.WorkspaceID, []json.RawMessage{eventJSON}); writeErr != nil {
		s.log.Errorn("Failed to store profile sync event in gateway DB",
			logger.NewIntField("segmentID", profile.Segment.ID),
			logger.NewStringField("error", writeErr.Error()),
		)
		return writeErr
	}

	s.log.Infon("Profile sync event stored in gateway DB for pipeline delivery",
		logger.NewIntField("segmentID", profile.Segment.ID),
		logger.NewStringField("workspaceID", profile.Segment.WorkspaceID),
		logger.NewIntField("traitCount", int64(len(profile.Traits))),
		logger.NewIntField("externalIDCount", int64(len(profile.ExternalIDs))),
	)

	if s.statsFactory != nil {
		s.statsFactory.NewTaggedStat(
			"identity_sync_events_sent",
			stats.CountType,
			stats.Tags{"workspaceID": profile.Segment.WorkspaceID},
		).Increment()
	}

	return nil
}

// SendBatch sends a batch of profile updates. Each profile is converted to an
// identify event and batched per workspace for efficient writing.
func (s *GatewayDestinationSender) SendBatch(ctx context.Context, profiles []*storage.ProfileData) error {
	if s.writer == nil {
		s.log.Warnn("No gateway event writer configured; batch profile sync not delivered",
			logger.NewIntField("batchSize", int64(len(profiles))),
		)
		return nil
	}

	// Group by workspace for efficient writing.
	byWorkspace := make(map[string][]json.RawMessage)
	for _, p := range profiles {
		if p == nil {
			continue
		}
		event := s.buildIdentifyEvent(p)
		eventJSON, err := jsonrs.Marshal(event)
		if err != nil {
			s.log.Warnn("Failed to marshal profile sync event, skipping",
				logger.NewIntField("segmentID", p.Segment.ID),
				logger.NewStringField("error", err.Error()),
			)
			continue
		}
		byWorkspace[p.Segment.WorkspaceID] = append(byWorkspace[p.Segment.WorkspaceID], eventJSON)
	}

	var lastErr error
	for wsID, events := range byWorkspace {
		if err := s.writer(ctx, wsID, events); err != nil {
			s.log.Errorn("Failed to store batch profile sync events",
				logger.NewStringField("workspaceID", wsID),
				logger.NewIntField("count", int64(len(events))),
				logger.NewStringField("error", err.Error()),
			)
			lastErr = err
		}
	}

	return lastErr
}

// buildIdentifyEvent converts a ProfileData into a RudderStack identify event.
func (s *GatewayDestinationSender) buildIdentifyEvent(profile *storage.ProfileData) map[string]any {
	traits := make(map[string]any, len(profile.Traits))
	for _, t := range profile.Traits {
		traits[t.Key] = t.Value
	}

	// Build external IDs array matching the Segment Unify spec: {type, id}.
	externalIDs := make([]map[string]string, 0, len(profile.ExternalIDs))
	var primaryUserID string
	for _, eid := range profile.ExternalIDs {
		externalIDs = append(externalIDs, map[string]string{
			"type": eid.ExternalIDType,
			"id":   eid.ExternalIDValue,
		})
		if eid.ExternalIDType == "user_id" && primaryUserID == "" {
			primaryUserID = eid.ExternalIDValue
		}
	}

	// Fall back to segment ID if no user_id is found.
	if primaryUserID == "" {
		primaryUserID = fmt.Sprintf("segment_%d", profile.Segment.ID)
	}

	return map[string]any{
		"type":              "identify",
		"userId":            primaryUserID,
		"traits":            traits,
		"messageId":         uuid.New().String(),
		"timestamp":         time.Now().UTC().Format(time.RFC3339),
		"originalTimestamp": time.Now().UTC().Format(time.RFC3339),
		"context": map[string]any{
			"profileSync": true,
			"segmentId":   profile.Segment.ID,
			"externalIds": externalIDs,
			"library": map[string]string{
				"name":    "rudder-server/identity-sync",
				"version": "1.0.0",
			},
		},
	}
}
