package cloudsources

import (
	"context"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
)

// PollFunc is the function type that connector implementations provide
// to execute the actual API call during each polling cycle. It receives
// the current pagination cursor and returns the fetched events, the next
// cursor position, and any error encountered.
//
// The cursor format is source-specific — it may be a timestamp string,
// a numeric offset, a page token, or any other opaque pagination marker.
// When the cursor is empty, the implementation should start from the
// beginning or use the source's default start position.
//
// Implementations must respect the provided context for cancellation.
// Long-running API calls should check ctx.Done() periodically.
type PollFunc func(ctx context.Context, cursor string) (events []Event, nextCursor string, err error)

// defaultEventsChannelCapacity is the buffer size for the Events channel.
// A buffered channel prevents the polling goroutine from blocking when
// the consumer is temporarily slower than the producer.
const defaultEventsChannelCapacity = 100

// BasePoller provides a default implementation of the Poller interface.
// It manages cursor-based pagination, configurable polling intervals,
// and context-aware cancellation for rate-limited API polling.
//
// BasePoller is designed as the foundation for polling-based cloud source
// connectors (e.g., Salesforce, HubSpot). Connector implementations
// provide a PollFunc that performs the actual API call; BasePoller handles
// the lifecycle management, interval timing, cursor state, and error logging.
//
// The polling loop follows the pattern established by
// services/transformer/features_impl.go (syncTransformerFeatureJson):
//   - Immediate initial poll on Start
//   - Configurable interval between subsequent polls via time.Ticker
//   - Context-aware cancellation at every blocking point
//   - Structured error logging with retry-on-next-tick semantics
//
// Thread Safety:
// Cursor state (SetCursor/GetCursor) is protected by sync.RWMutex for
// safe concurrent access from the background polling goroutine and
// external monitoring callers.
type BasePoller struct {
	// Config holds the polling configuration including interval, max retries,
	// rate limit, initial cursor, page size, and timeout settings.
	Config PollingConfig

	// Logger is the structured logger used for error and status reporting.
	// Uses the rudder-go-kit logger interface with Errorn/Infon methods.
	Logger logger.Logger

	// Events is a buffered channel through which polled events are delivered
	// to the consumer. The channel capacity is set during construction
	// (default: 100 batches). Each element is a slice of Event instances
	// from a single poll cycle.
	Events chan []Event

	// cursor holds the current pagination position for the next poll cycle.
	// Access is protected by cursorMu.
	cursor string

	// cursorMu protects concurrent read/write access to the cursor field.
	cursorMu sync.RWMutex

	// pollFunc is the connector-provided function that performs the actual
	// API call during each polling cycle.
	pollFunc PollFunc

	// running tracks whether the background polling loop is currently active.
	running bool

	// runningMu protects concurrent access to the running field.
	runningMu sync.RWMutex
}

// NewBasePoller creates a new BasePoller with the given polling configuration,
// poll function, and logger. It initializes the Events channel with a default
// buffer capacity and sets the initial cursor from the configuration.
//
// If the polling interval in cfg is zero or negative, it defaults to
// DefaultPollingInterval (5 minutes) to prevent tight-loop polling.
//
// Parameters:
//   - cfg: Polling configuration (interval, retries, rate limit, initial cursor)
//   - pollFn: The function that executes the actual API call per cycle
//   - log: Structured logger for error and status reporting
//
// Returns a ready-to-use *BasePoller. Call Start to begin the polling loop.
func NewBasePoller(cfg PollingConfig, pollFn PollFunc, log logger.Logger) *BasePoller {
	// Apply default interval if not configured
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultPollingInterval
	}

	return &BasePoller{
		Config:   cfg,
		Logger:   log,
		Events:   make(chan []Event, defaultEventsChannelCapacity),
		cursor:   cfg.InitialCursor,
		pollFunc: pollFn,
	}
}

// Poll executes a single polling cycle by calling the configured PollFunc
// with the current cursor. On success, it updates the cursor to the next
// position and returns the fetched events. On error, it returns nil events
// and the error without modifying the cursor.
//
// Poll respects context cancellation — it checks ctx.Err() before invoking
// the PollFunc and returns the context error immediately if the context
// has been cancelled. This ensures rapid shutdown when the parent context
// is cancelled during the polling loop.
//
// This method implements the Poller interface's Poll contract.
func (p *BasePoller) Poll(ctx context.Context) ([]Event, error) {
	// Check for context cancellation before starting the poll cycle
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Get the current cursor under read lock
	currentCursor := p.GetCursor()

	// Execute the connector-provided poll function
	events, nextCursor, err := p.pollFunc(ctx, currentCursor)
	if err != nil {
		return nil, err
	}

	// Check for context cancellation after the poll function returns
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Update cursor to the next position if a new cursor was provided
	if nextCursor != "" {
		p.SetCursor(nextCursor)
	}

	return events, nil
}

// Start begins the background polling loop. It performs an immediate initial
// poll, then polls at the configured interval until the context is cancelled.
//
// The polling loop follows the pattern from services/transformer/features_impl.go
// (syncTransformerFeatureJson method):
//   - Immediate initial poll on entry
//   - time.Ticker for subsequent polls at Config.Interval
//   - Context-aware cancellation checked at every blocking select
//   - Errors logged via Logger.Errorn with structured fields; polling continues
//     on the next tick (retry-on-next-tick semantics)
//   - Events delivered through the Events channel; if the channel blocks,
//     the poller waits but respects context cancellation
//
// Start is a blocking call — it runs until the context is cancelled. Callers
// should invoke it in a separate goroutine (e.g., go poller.Start(ctx)).
//
// Returns the context error when the context is cancelled, or nil if Stop
// is called before context cancellation.
func (p *BasePoller) Start(ctx context.Context) error {
	p.setRunning(true)
	defer p.setRunning(false)

	ticker := time.NewTicker(p.Config.Interval)
	defer ticker.Stop()

	// Perform an immediate initial poll before entering the ticker loop.
	// This mirrors the syncTransformerFeatureJson pattern where the first
	// fetch happens immediately, not after waiting for the first tick.
	events, err := p.Poll(ctx)
	if err != nil {
		p.Logger.Errorn("initial poll failed", obskit.Error(err))
	} else if len(events) > 0 {
		select {
		case p.Events <- events:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Main polling loop: poll on each tick, respect context cancellation.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			events, err := p.Poll(ctx)
			if err != nil {
				p.Logger.Errorn("poll cycle failed", obskit.Error(err))
				continue // retry on next tick
			}
			if len(events) > 0 {
				select {
				case p.Events <- events:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
}

// Stop gracefully shuts down the poller by setting the running flag to false
// and closing the Events channel. After Stop is called, no further events
// will be sent through the Events channel.
//
// Stop should only be called after the Start goroutine has returned (i.e.,
// after the context has been cancelled). Calling Stop while Start is still
// running may cause a panic from sending on a closed channel; prefer
// cancelling the context to signal the polling loop to stop.
func (p *BasePoller) Stop() error {
	p.setRunning(false)
	close(p.Events)
	return nil
}

// SetCursor stores the pagination cursor for the next polling cycle.
// The cursor format is source-specific (e.g., timestamp, offset, page token).
//
// This method is thread-safe — it acquires a write lock on the cursor mutex
// to protect against concurrent access from the background polling goroutine
// and external callers (e.g., monitoring or persistence goroutines).
func (p *BasePoller) SetCursor(cursor string) {
	p.cursorMu.Lock()
	defer p.cursorMu.Unlock()
	p.cursor = cursor
}

// GetCursor retrieves the current pagination cursor. Returns an empty string
// if no cursor has been set (i.e., the poller starts from the beginning on
// the next poll cycle).
//
// This method is thread-safe — it acquires a read lock on the cursor mutex
// to allow concurrent reads from monitoring goroutines while the polling
// loop may be writing a new cursor value.
func (p *BasePoller) GetCursor() string {
	p.cursorMu.RLock()
	defer p.cursorMu.RUnlock()
	return p.cursor
}

// IsRunning returns whether the background polling loop is currently active.
// Returns true after Start is called and before the context is cancelled or
// Stop is called.
//
// This method is thread-safe — it acquires a read lock on the running mutex
// to ensure a consistent view of the running state from monitoring goroutines.
func (p *BasePoller) IsRunning() bool {
	p.runningMu.RLock()
	defer p.runningMu.RUnlock()
	return p.running
}

// setRunning is an internal helper that updates the running flag under a
// write lock. Used by Start and Stop to safely transition the running state.
func (p *BasePoller) setRunning(v bool) {
	p.runningMu.Lock()
	defer p.runningMu.Unlock()
	p.running = v
}
