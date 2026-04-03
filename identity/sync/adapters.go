// Package sync — adapters.go provides ready-to-use adapter implementations of the
// ChangeListener, ProfileAssembler, and DestinationSender interfaces (E-029).
//
// These adapters bridge the identity graph service and storage layer with the
// Syncer's interface contracts, enabling startup wiring in
// app/apphandlers/embeddedAppHandler.go without circular imports.
//
// Design: Adapters accept function parameters (not concrete types) so the sync
// package never imports identity/graph directly, keeping the dependency graph
// unidirectional: graph → storage ← sync.
package sync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/identity/storage"
)

// ---------------------------------------------------------------------------
// ProfileAssembler adapter
// ---------------------------------------------------------------------------

// ProfileDataFunc is a function that retrieves a complete ProfileData snapshot
// for a given segment ID. This matches the signature of
// identity/graph.IdentityGraph.GetProfileData.
type ProfileDataFunc func(ctx context.Context, segmentID int64) (*storage.ProfileData, error)

// FuncProfileAssembler adapts a ProfileDataFunc to the ProfileAssembler interface.
// This avoids importing the identity/graph package directly.
//
// Usage in embeddedAppHandler.go:
//
//	assembler := identitysync.NewFuncProfileAssembler(graphSvc.GetProfileData)
type FuncProfileAssembler struct {
	fn ProfileDataFunc
}

// NewFuncProfileAssembler creates a ProfileAssembler that delegates to fn.
// Panics if fn is nil.
func NewFuncProfileAssembler(fn ProfileDataFunc) *FuncProfileAssembler {
	if fn == nil {
		panic("identity/sync: NewFuncProfileAssembler requires non-nil function")
	}
	return &FuncProfileAssembler{fn: fn}
}

// AssembleProfile implements ProfileAssembler by calling the wrapped function.
func (a *FuncProfileAssembler) AssembleProfile(ctx context.Context, segmentID int64) (*storage.ProfileData, error) {
	return a.fn(ctx, segmentID)
}

// ---------------------------------------------------------------------------
// ChangeListener adapter — in-memory channel-based
// ---------------------------------------------------------------------------

// ChannelChangeListener implements ChangeListener using an in-memory channel.
// It is designed to be fed by the identity graph service when events are processed,
// providing real-time CDC without polling.
//
// Usage:
//
//	listener := identitysync.NewChannelChangeListener(1000, log)
//	graphSvc.SetChangeEmitter(listener.Emit) // wire event emission
//	syncer, _ := identitysync.New(listener, assembler, sender, conf, log, stats)
type ChannelChangeListener struct {
	mu         sync.Mutex
	ch         chan ChangeEvent
	log        logger.Logger
	checkpoint int64
}

// NewChannelChangeListener creates a buffered channel-based ChangeListener.
// bufferSize controls the channel buffer; events are dropped with a warning
// if the buffer is full (back-pressure safety).
func NewChannelChangeListener(bufferSize int, log logger.Logger) *ChannelChangeListener {
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	if log == nil {
		log = pkgLogger
	}
	return &ChannelChangeListener{
		ch:  make(chan ChangeEvent, bufferSize),
		log: log.Child("change-listener"),
	}
}

// Emit publishes a ChangeEvent to the listener's channel.
// If the channel buffer is full, the event is dropped with a warning log.
// This method is safe for concurrent use.
func (l *ChannelChangeListener) Emit(event ChangeEvent) {
	select {
	case l.ch <- event:
	default:
		l.log.Warnn("Change event dropped — buffer full",
			logger.NewIntField("segmentID", event.SegmentID),
			logger.NewStringField("changeType", event.Type.String()),
		)
	}
}

// Subscribe implements ChangeListener. Returns a read-only channel of ChangeEvents.
// The channel is derived from the internal buffered channel; it remains open until
// the context is cancelled, at which point a goroutine drains and closes a wrapper
// channel so the Syncer's select loop can detect shutdown.
func (l *ChannelChangeListener) Subscribe(ctx context.Context) (<-chan ChangeEvent, error) {
	out := make(chan ChangeEvent, cap(l.ch))
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-l.ch:
				if !ok {
					return
				}
				select {
				case out <- evt:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Checkpoint implements ChangeListener. Records the last successfully processed
// event ID so that after a restart the Syncer can resume from this point.
// In this in-memory implementation the checkpoint is stored locally; a durable
// implementation would persist to PostgreSQL.
func (l *ChannelChangeListener) Checkpoint(_ context.Context, eventID int64) error {
	l.mu.Lock()
	l.checkpoint = eventID
	l.mu.Unlock()
	return nil
}

// LastCheckpoint returns the last checkpointed event ID (for testing/diagnostics).
func (l *ChannelChangeListener) LastCheckpoint() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checkpoint
}

// ---------------------------------------------------------------------------
// DestinationSender adapter — logging-only (initial wiring)
// ---------------------------------------------------------------------------

// LogDestinationSender implements DestinationSender by logging profile deliveries.
// This is the initial wiring implementation for E-029 startup integration.
// In production, this would be replaced with a Router-backed sender that delivers
// profile updates to configured downstream destinations.
type LogDestinationSender struct {
	log logger.Logger
}

// NewLogDestinationSender creates a DestinationSender that logs profile deliveries.
func NewLogDestinationSender(log logger.Logger) *LogDestinationSender {
	if log == nil {
		log = pkgLogger
	}
	return &LogDestinationSender{log: log.Child("destination-sender")}
}

// SendProfile implements DestinationSender for a single profile.
func (s *LogDestinationSender) SendProfile(_ context.Context, profile *storage.ProfileData) error {
	if profile == nil {
		return fmt.Errorf("identity sync sender: nil profile")
	}
	s.log.Infon("Profile sync — delivering profile update",
		logger.NewIntField("segmentID", profile.Segment.ID),
		logger.NewStringField("workspaceID", profile.Segment.WorkspaceID),
		logger.NewIntField("externalIDCount", int64(len(profile.ExternalIDs))),
		logger.NewIntField("traitCount", int64(len(profile.Traits))),
		logger.NewStringField("timestamp", time.Now().UTC().Format(time.RFC3339)),
	)
	return nil
}

// SendBatch implements DestinationSender for a batch of profiles.
func (s *LogDestinationSender) SendBatch(ctx context.Context, profiles []*storage.ProfileData) error {
	for _, p := range profiles {
		if err := s.SendProfile(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
