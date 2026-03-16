package cloudsources

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
)

// replayProtectionWindow is the duration for which event IDs are tracked
// to detect and reject duplicate webhook deliveries. Events with the same
// ID received within this window are considered replays and are silently
// dropped with a warning log.
const replayProtectionWindow = 5 * time.Minute

// BaseWebhookReceiver provides a default implementation of the WebhookReceiver interface.
// It handles HMAC signature validation, payload normalization, and replay protection.
// This is the foundation for webhook-based cloud source connectors like Stripe,
// SendGrid, GitHub, and other services that deliver events via HTTP callbacks.
//
// Security features:
//   - HMAC-SHA256 signature validation with constant-time comparison (hmac.Equal)
//   - Replay protection via a time-windowed seen-events map to prevent duplicate processing
//   - Request body preservation: the body is read and restored after validation so that
//     Transform can re-read it without error
//
// Thread safety: All methods are safe for concurrent use from multiple HTTP handler
// goroutines. The internal seenEvents map is protected by a sync.RWMutex.
//
// This is a proof-of-concept implementation (Sprint 2–3, E-009). It demonstrates
// the webhook receiver pattern for cloud source connectors but is not intended
// for production use without additional hardening (persistent deduplication,
// distributed replay protection, graceful shutdown coordination).
type BaseWebhookReceiver struct {
	// HMACSecret is the shared secret used for HMAC-SHA256 signature validation.
	// It must never be hardcoded — inject at runtime from encrypted storage.
	// When empty, signature validation is skipped (useful for local development only).
	HMACSecret string

	// SignatureHeader is the HTTP header name containing the HMAC signature
	// on inbound webhook requests. Source-specific examples:
	//   - Stripe: "Stripe-Signature"
	//   - SendGrid: "X-Twilio-Email-Event-Webhook-Signature"
	//   - GitHub: "X-Hub-Signature-256"
	//   - Default: "X-Webhook-Signature"
	SignatureHeader string

	// Logger provides structured logging for HMAC validation failures,
	// replay protection warnings, and transform errors. Uses the
	// github.com/rudderlabs/rudder-go-kit/logger interface for consistency
	// with the rest of the RudderStack codebase.
	Logger logger.Logger

	// SchemaMapper transforms raw cloud source events into Segment Spec events.
	// Each connector provides its own SchemaMapper implementation for
	// source-specific data format handling. Must not be nil when Transform is called.
	SchemaMapper SchemaMapper

	// sourceType identifies the cloud source connector type for event metadata.
	// Set from the constructor context; used when building intermediate Event structs.
	sourceType string

	// seenEvents tracks recently processed event IDs for replay protection.
	// Keys are event IDs (from the webhook payload "id", "event_id", or
	// "idempotency_key" fields), values are the timestamp when the event was first seen.
	seenEvents map[string]time.Time

	// seenEventsMu protects concurrent access to the seenEvents map from
	// multiple HTTP handler goroutines processing webhook requests simultaneously.
	seenEventsMu sync.RWMutex
}

// NewBaseWebhookReceiver creates a new BaseWebhookReceiver configured from
// the provided WebhookConfig, SchemaMapper, and Logger. It initializes the
// replay protection map and sets default values for unspecified configuration fields.
//
// Parameters:
//   - cfg: WebhookConfig containing HMAC secret, signature header, and validation settings.
//     If cfg.SignatureHeader is empty, it defaults to "X-Webhook-Signature".
//   - mapper: SchemaMapper for transforming webhook payloads into Segment Spec events.
//   - log: Logger for structured logging of validation and transform operations.
//
// Returns a fully initialized *BaseWebhookReceiver ready to handle webhook requests.
func NewBaseWebhookReceiver(cfg WebhookConfig, mapper SchemaMapper, log logger.Logger) *BaseWebhookReceiver {
	signatureHeader := cfg.SignatureHeader
	if signatureHeader == "" {
		signatureHeader = "X-Webhook-Signature"
	}

	return &BaseWebhookReceiver{
		HMACSecret:      cfg.HMACSecret,
		SignatureHeader:  signatureHeader,
		Logger:          log,
		SchemaMapper:    mapper,
		seenEvents:      make(map[string]time.Time),
	}
}

// Validate verifies the authenticity of an incoming webhook request by checking
// the HMAC-SHA256 signature against the shared secret. It implements the
// WebhookReceiver interface defined in cloud_source.go.
//
// The validation flow:
//  1. If HMACSecret is empty, returns (true, nil) — no validation is configured.
//  2. Reads the signature from the HTTP header specified by SignatureHeader.
//  3. If the signature header is missing, returns (false, nil) with a warning log.
//  4. Reads the request body and restores it via io.NopCloser(bytes.NewReader(body))
//     so that subsequent Transform calls can re-read the body.
//  5. Delegates to ValidateSignature for HMAC-SHA256 comparison with constant-time hmac.Equal.
//
// Returns:
//   - (true, nil) if the signature is valid or no validation is configured
//   - (false, nil) if the signature is missing or invalid
//   - (false, error) if the request body cannot be read
func (recv *BaseWebhookReceiver) Validate(r *http.Request) (bool, error) {
	// If no HMAC secret is configured, skip validation entirely.
	// This allows local development without requiring webhook secrets.
	if recv.HMACSecret == "" {
		return true, nil
	}

	// Read the signature from the configured HTTP header
	signature := r.Header.Get(recv.SignatureHeader)
	if signature == "" {
		recv.Logger.Warnn("webhook signature header missing",
			logger.NewStringField("header", recv.SignatureHeader),
			logger.NewStringField("remoteAddr", r.RemoteAddr),
		)
		return false, nil
	}

	// Read the request body for signature computation.
	// The body must be fully consumed and then restored so that Transform
	// can read it again after validation succeeds.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, fmt.Errorf("failed to read request body for signature validation: %w", err)
	}
	// Restore the request body using bytes.NewReader wrapped in io.NopCloser
	r.Body = io.NopCloser(bytes.NewReader(body))

	// Validate the signature using HMAC-SHA256 with constant-time comparison
	valid := recv.ValidateSignature(body, signature)
	if !valid {
		recv.Logger.Warnn("webhook HMAC signature validation failed",
			logger.NewStringField("header", recv.SignatureHeader),
			logger.NewStringField("remoteAddr", r.RemoteAddr),
		)
	}

	return valid, nil
}

// Transform converts a validated webhook request into Segment Spec events.
// It implements the WebhookReceiver interface defined in cloud_source.go.
//
// The transformation flow:
//  1. Reads the request body using io.ReadAll.
//  2. Unmarshals the JSON payload into a map[string]interface{} using jsonrs.Unmarshal.
//  3. Extracts the event ID from the payload (tries "id", "event_id", "idempotency_key" fields).
//  4. If an event ID is found, checks against the replay protection window (5 minutes).
//     Duplicate events are silently dropped with a warning log.
//  5. Constructs an intermediate Event struct from the webhook payload.
//  6. Delegates to SchemaMapper.MapToSegmentSpec(event) to produce SegmentEvents.
//
// Returns:
//   - A non-nil slice of SegmentEvent on success (may be empty for replays)
//   - An error if the body cannot be read, parsed, or mapped
func (recv *BaseWebhookReceiver) Transform(r *http.Request) ([]SegmentEvent, error) {
	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body for transformation: %w", err)
	}

	// Parse the JSON payload using jsonrs (mandated by depguard — not encoding/json)
	var payload map[string]interface{}
	if err := jsonrs.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal webhook payload: %w", err)
	}

	// Extract event ID for replay protection from common field names
	eventID := extractStringField(payload, "id", "event_id", "idempotency_key")

	// Check for replay if an event ID is present
	if eventID != "" {
		if recv.isReplay(eventID) {
			recv.Logger.Warnn("replay detected, skipping duplicate webhook event",
				logger.NewStringField("eventID", eventID),
			)
			return []SegmentEvent{}, nil
		}
		recv.markSeen(eventID)
	}

	// Extract event type from common field names used by webhook providers
	eventType := extractStringField(payload, "type", "event", "event_type")

	// Extract event name — try name-specific fields first, then fall back to type
	eventName := extractStringField(payload, "name", "event", "event_type", "type")

	// Extract or generate timestamp from the webhook payload.
	// Supports RFC3339 string timestamps and Unix numeric timestamps (e.g., Stripe "created").
	timestamp := time.Now()
	if ts, ok := payload["timestamp"]; ok {
		if tsStr, ok := ts.(string); ok {
			if parsed, parseErr := time.Parse(time.RFC3339, tsStr); parseErr == nil {
				timestamp = parsed
			}
		} else if tsFloat, ok := ts.(float64); ok {
			timestamp = time.Unix(int64(tsFloat), 0)
		}
	} else if created, ok := payload["created"]; ok {
		// Stripe and similar providers use "created" as a Unix epoch timestamp
		if createdFloat, ok := created.(float64); ok {
			timestamp = time.Unix(int64(createdFloat), 0)
		}
	}

	// Extract user ID from common user identification fields
	userID := extractStringField(payload, "userId", "user_id", "customer_id", "customer", "email")

	// Construct the intermediate cloud source Event for schema mapping
	event := Event{
		ID:         eventID,
		Type:       eventType,
		Name:       eventName,
		SourceType: recv.sourceType,
		Timestamp:  timestamp,
		Data:       payload,
		UserID:     userID,
	}

	// Delegate to SchemaMapper to produce Segment Spec events
	if recv.SchemaMapper == nil {
		return nil, fmt.Errorf("schema mapper is not configured on BaseWebhookReceiver")
	}

	segmentEvents, err := recv.SchemaMapper.MapToSegmentSpec(event)
	if err != nil {
		return nil, fmt.Errorf("schema mapping failed for event %q: %w", eventID, err)
	}

	return segmentEvents, nil
}

// ValidateSignature performs HMAC-SHA256 signature validation on a raw payload
// without requiring HTTP context. This is an exported helper method suitable for
// use by both the Validate method and by connector-specific implementations
// that need custom signature extraction logic (e.g., Stripe's "v1=..." multi-field
// signature format).
//
// The algorithm:
//  1. Compute HMAC-SHA256(HMACSecret, payload) using hmac.New(sha256.New, secret)
//  2. Encode the expected MAC to hex using hex.EncodeToString for comparison
//  3. Attempt to hex-decode the provided signature using hex.DecodeString
//  4. If hex decoding succeeds, compare raw bytes using constant-time hmac.Equal
//  5. If hex decoding fails (provider uses non-hex format), fall back to
//     constant-time comparison of hex-encoded strings
//
// Returns true if the signatures match, false otherwise.
func (recv *BaseWebhookReceiver) ValidateSignature(payload []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(recv.HMACSecret))
	if _, err := mac.Write(payload); err != nil {
		return false
	}
	expectedMAC := mac.Sum(nil)

	// Encode the expected MAC to hex string for potential string-based comparison
	expectedHex := hex.EncodeToString(expectedMAC)

	// Try to decode the provided signature from hex encoding
	providedMAC, err := hex.DecodeString(signature)
	if err != nil {
		// If the provided signature is not valid hex, fall back to constant-time
		// comparison of the hex-encoded strings directly. This handles providers
		// that may use different encoding formats or include prefixes.
		return hmac.Equal([]byte(expectedHex), []byte(signature))
	}

	// Compare raw MAC bytes using constant-time comparison to prevent timing attacks
	return hmac.Equal(expectedMAC, providedMAC)
}

// ServeHTTP is a convenience HTTP handler that combines Validate and Transform
// into a single endpoint. It implements the http.Handler interface, allowing
// BaseWebhookReceiver to be used directly as an HTTP handler in Chi router
// registration or any standard http.Handler-compatible middleware chain.
//
// Response codes:
//   - 200 OK: Webhook processed successfully; SegmentEvents returned as JSON body
//   - 401 Unauthorized: HMAC signature validation failed or signature header missing
//   - 400 Bad Request: Request body read error, payload parse error, or transform failure
//
// The response body for 200 OK is a JSON array of SegmentEvent objects serialized
// using jsonrs.Marshal (mandated by depguard linting rule).
func (recv *BaseWebhookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 1: Validate the webhook signature
	valid, err := recv.Validate(r)
	if err != nil {
		recv.Logger.Errorn("webhook validation error",
			logger.NewStringField("error", err.Error()),
			logger.NewStringField("remoteAddr", r.RemoteAddr),
		)
		http.Error(w, "validation error", http.StatusBadRequest)
		return
	}
	if !valid {
		http.Error(w, "unauthorized: invalid webhook signature", http.StatusUnauthorized)
		return
	}

	// Step 2: Transform the webhook payload into Segment Spec events
	events, err := recv.Transform(r)
	if err != nil {
		recv.Logger.Errorn("webhook transform error",
			logger.NewStringField("error", err.Error()),
			logger.NewStringField("remoteAddr", r.RemoteAddr),
		)
		http.Error(w, fmt.Errorf("transform error: %w", err).Error(), http.StatusBadRequest)
		return
	}

	// Step 3: Serialize and return the SegmentEvents as JSON
	responseBody, err := jsonrs.Marshal(events)
	if err != nil {
		recv.Logger.Errorn("failed to marshal webhook response",
			logger.NewStringField("error", err.Error()),
		)
		http.Error(w, "internal serialization error", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, writeErr := w.Write(responseBody); writeErr != nil {
		recv.Logger.Errorn("failed to write webhook response",
			logger.NewStringField("error", writeErr.Error()),
		)
	}
}

// cleanupSeenEvents removes entries older than the replay protection window
// from the seenEvents map. This prevents unbounded memory growth from the
// replay protection mechanism. It acquires a write lock on seenEventsMu.
//
// The cleanup is called before each replay check in isReplay, ensuring that
// stale entries are removed on every incoming webhook request. For production
// use, this should be replaced with a background goroutine or an LRU cache.
func (recv *BaseWebhookReceiver) cleanupSeenEvents() {
	cutoff := time.Now().Add(-replayProtectionWindow)
	recv.seenEventsMu.Lock()
	defer recv.seenEventsMu.Unlock()
	for id, seenAt := range recv.seenEvents {
		if seenAt.Before(cutoff) {
			delete(recv.seenEvents, id)
		}
	}
}

// isReplay checks whether the given event ID has been seen within the replay
// protection window (5 minutes). It first cleans up expired entries, then
// checks for the event ID in the seenEvents map.
//
// Returns true if the event is a duplicate (already seen), false otherwise.
func (recv *BaseWebhookReceiver) isReplay(eventID string) bool {
	// Clean up expired entries before checking
	recv.cleanupSeenEvents()

	recv.seenEventsMu.RLock()
	defer recv.seenEventsMu.RUnlock()
	_, seen := recv.seenEvents[eventID]
	return seen
}

// markSeen records the given event ID in the seen events map with the current
// timestamp. Subsequent calls to isReplay with the same ID within the replay
// protection window will return true, causing the duplicate event to be dropped.
func (recv *BaseWebhookReceiver) markSeen(eventID string) {
	recv.seenEventsMu.Lock()
	defer recv.seenEventsMu.Unlock()
	recv.seenEvents[eventID] = time.Now()
}

// extractStringField extracts a string value from a map by trying multiple
// field names in priority order. Returns the first non-empty string value found,
// or an empty string if no matching field contains a non-empty string value.
//
// This utility supports the varied field naming conventions used by different
// webhook providers (e.g., "id" vs "event_id" vs "idempotency_key" for event
// identifiers, "type" vs "event" vs "event_type" for event classification).
func extractStringField(data map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if val, ok := data[key]; ok {
			if strVal, ok := val.(string); ok && strVal != "" {
				return strVal
			}
		}
	}
	return ""
}
