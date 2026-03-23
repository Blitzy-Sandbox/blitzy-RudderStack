// Package stripe implements a Stripe webhook connector proof-of-concept
// for the cloud source ingestion framework (E-009). It demonstrates the
// WebhookReceiver, SchemaMapper, and CloudSource interface patterns by
// handling Stripe webhook events and mapping them to Segment Spec events.
//
// Supported Stripe events:
//   - charge.succeeded → track "Payment Completed"
//   - customer.created → identify (customer traits)
//   - invoice.paid → track "Invoice Paid"
//   - customer.subscription.created → track "Subscription Created"
//   - customer.subscription.deleted → track "Subscription Cancelled"
//
// This is a proof-of-concept only — not production-grade.
package stripe

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	cloudsources "github.com/rudderlabs/rudder-server/services/cloud-sources"
)

const (
	// ConnectorName is the registered name for this connector.
	ConnectorName = "stripe"

	// stripeSignatureHeader is the HTTP header used by Stripe for webhook signatures.
	stripeSignatureHeader = "Stripe-Signature"

	// timestampTolerance is the maximum age of a webhook signature before it's rejected.
	// Stripe recommends 5 minutes (300 seconds).
	timestampTolerance = 300 * time.Second

	// libraryName identifies this connector in Segment context.library.
	libraryName = "rudder-cloud-sources"

	// libraryVersion is the proof-of-concept version.
	libraryVersion = "0.1.0"
)

// Stripe event type → Segment event mapping constants.
const (
	eventChargeSucceeded     = "charge.succeeded"
	eventCustomerCreated     = "customer.created"
	eventInvoicePaid         = "invoice.paid"
	eventSubscriptionCreated = "customer.subscription.created"
	eventSubscriptionDeleted = "customer.subscription.deleted"
)

// stripeWebhookEvent represents the structure of a Stripe webhook event payload.
type stripeWebhookEvent struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		Object map[string]interface{} `json:"object"`
	} `json:"data"`
}

// StripeConnector implements the cloud source framework interfaces for
// Stripe webhook-based event ingestion. It validates HMAC-SHA256 signatures,
// transforms Stripe webhook events into Segment Spec events, and manages
// the connector lifecycle.
type StripeConnector struct {
	config     cloudsources.CloudSourceConfig
	webhookCfg cloudsources.WebhookConfig
	logger     logger.Logger
	mapper     *StripeSchemaMapper
	healthy    bool
}

// NewStripeConnector creates a new StripeConnector from the provided config.
// It extracts the webhook configuration, initializes a child logger, and
// creates a schema mapper for event transformation.
func NewStripeConnector(cfg cloudsources.CloudSourceConfig) (*StripeConnector, error) {
	return &StripeConnector{
		config:     cfg,
		webhookCfg: cfg.Webhook,
		logger:     logger.NewLogger().Child("cloud-source-stripe"),
		mapper:     NewStripeSchemaMapper(),
		healthy:    false,
	}, nil
}

// Start marks the connector as healthy and logs the startup event.
// For this webhook-based PoC, no background goroutines are needed.
func (c *StripeConnector) Start(_ context.Context) error {
	c.healthy = true
	c.logger.Infon("stripe connector started",
		logger.NewStringField("sourceId", c.config.ID))
	return nil
}

// Stop marks the connector as unhealthy and logs the shutdown event.
func (c *StripeConnector) Stop(_ context.Context) error {
	c.healthy = false
	c.logger.Infon("stripe connector stopped")
	return nil
}

// Status returns the current health status of the Stripe connector.
func (c *StripeConnector) Status() cloudsources.SourceStatus {
	msg := "stopped"
	if c.healthy {
		msg = "operational"
	}
	return cloudsources.SourceStatus{
		Name:    ConnectorName,
		Healthy: c.healthy,
		Message: msg,
	}
}

// Validate verifies the HMAC-SHA256 signature on an incoming Stripe webhook request.
// It reads the Stripe-Signature header, parses the timestamp and signature(s),
// checks timestamp tolerance, computes the expected HMAC, and performs a
// constant-time comparison. The request body is read and restored for
// subsequent processing by Transform.
func (c *StripeConnector) Validate(r *http.Request) (bool, error) {
	sigHeader := r.Header.Get(stripeSignatureHeader)
	if sigHeader == "" {
		return false, fmt.Errorf("missing %s header", stripeSignatureHeader)
	}

	timestamp, signatures, err := parseStripeSignature(sigHeader)
	if err != nil {
		return false, err
	}

	// Read the request body and restore it for subsequent use by Transform
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, fmt.Errorf("failed to read request body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	// Check timestamp tolerance (5 minute window)
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false, fmt.Errorf("invalid timestamp in signature: %w", err)
	}
	if time.Since(time.Unix(ts, 0)) > timestampTolerance {
		return false, nil
	}

	// Compute expected HMAC-SHA256 signature
	signedPayload := timestamp + "." + string(body)
	mac := hmac.New(sha256.New, []byte(c.webhookCfg.HMACSecret))
	mac.Write([]byte(signedPayload))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	expectedBytes, _ := hex.DecodeString(expectedSig)

	// Constant-time comparison against all provided v1 signatures
	for _, sig := range signatures {
		providedBytes, decErr := hex.DecodeString(sig)
		if decErr != nil {
			continue
		}
		if hmac.Equal(expectedBytes, providedBytes) {
			return true, nil
		}
	}
	return false, nil
}

// Transform reads the Stripe webhook payload from the request body,
// unmarshals it into the internal Stripe event structure, converts it
// to the framework's Event type, and delegates to the schema mapper
// for Segment Spec event generation.
func (c *StripeConnector) Transform(r *http.Request) ([]cloudsources.SegmentEvent, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("empty request body")
	}

	var webhookEvent stripeWebhookEvent
	if err := jsonrs.Unmarshal(body, &webhookEvent); err != nil {
		return nil, fmt.Errorf("failed to unmarshal webhook payload: %w", err)
	}

	event := cloudsources.Event{
		ID:         webhookEvent.ID,
		Type:       mapStripeEventToSegmentType(webhookEvent.Type),
		Name:       webhookEvent.Type,
		SourceType: ConnectorName,
		Timestamp:  time.Unix(webhookEvent.Created, 0),
		Data:       webhookEvent.Data.Object,
		UserID:     extractCustomerID(webhookEvent.Data.Object),
	}

	return c.mapper.MapToSegmentSpec(event)
}

// StripeSchemaMapper maps Stripe webhook events to Segment Spec events.
// It implements the cloudsources.SchemaMapper interface.
type StripeSchemaMapper struct{}

// NewStripeSchemaMapper creates a new StripeSchemaMapper.
func NewStripeSchemaMapper() *StripeSchemaMapper {
	return &StripeSchemaMapper{}
}

// MapToSegmentSpec converts a cloud source Event into one or more Segment Spec
// events. It dispatches to event-specific mapping methods based on the Stripe
// event name.
func (m *StripeSchemaMapper) MapToSegmentSpec(event cloudsources.Event) ([]cloudsources.SegmentEvent, error) {
	switch event.Name {
	case eventChargeSucceeded:
		return m.mapChargeSucceeded(event)
	case eventCustomerCreated:
		return m.mapCustomerCreated(event)
	case eventInvoicePaid:
		return m.mapInvoicePaid(event)
	case eventSubscriptionCreated:
		return m.mapSubscriptionCreated(event)
	case eventSubscriptionDeleted:
		return m.mapSubscriptionDeleted(event)
	default:
		return m.mapDefaultEvent(event)
	}
}

// mapChargeSucceeded converts a Stripe charge.succeeded event to a Segment
// track event with Event="Payment Completed".
func (m *StripeSchemaMapper) mapChargeSucceeded(event cloudsources.Event) ([]cloudsources.SegmentEvent, error) {
	return []cloudsources.SegmentEvent{
		{
			Type:              "track",
			MessageID:         uuid.New().String(),
			UserID:            event.UserID,
			Event:             "Payment Completed",
			Properties:        event.Data,
			Context:           buildContext(),
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
			OriginalTimestamp: event.Timestamp.UTC().Format(time.RFC3339),
		},
	}, nil
}

// mapCustomerCreated converts a Stripe customer.created event to a Segment
// identify event with customer Traits (email, name, description).
func (m *StripeSchemaMapper) mapCustomerCreated(event cloudsources.Event) ([]cloudsources.SegmentEvent, error) {
	return []cloudsources.SegmentEvent{
		{
			Type:              "identify",
			MessageID:         uuid.New().String(),
			UserID:            event.UserID,
			Traits:            event.Data,
			Context:           buildContext(),
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
			OriginalTimestamp: event.Timestamp.UTC().Format(time.RFC3339),
		},
	}, nil
}

// mapInvoicePaid converts a Stripe invoice.paid event to a Segment
// track event with Event="Invoice Paid".
func (m *StripeSchemaMapper) mapInvoicePaid(event cloudsources.Event) ([]cloudsources.SegmentEvent, error) {
	return []cloudsources.SegmentEvent{
		{
			Type:              "track",
			MessageID:         uuid.New().String(),
			UserID:            event.UserID,
			Event:             "Invoice Paid",
			Properties:        event.Data,
			Context:           buildContext(),
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
			OriginalTimestamp: event.Timestamp.UTC().Format(time.RFC3339),
		},
	}, nil
}

// mapSubscriptionCreated converts a Stripe customer.subscription.created event
// to a Segment track event with Event="Subscription Created".
func (m *StripeSchemaMapper) mapSubscriptionCreated(event cloudsources.Event) ([]cloudsources.SegmentEvent, error) {
	return []cloudsources.SegmentEvent{
		{
			Type:              "track",
			MessageID:         uuid.New().String(),
			UserID:            event.UserID,
			Event:             "Subscription Created",
			Properties:        event.Data,
			Context:           buildContext(),
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
			OriginalTimestamp: event.Timestamp.UTC().Format(time.RFC3339),
		},
	}, nil
}

// mapSubscriptionDeleted converts a Stripe customer.subscription.deleted event
// to a Segment track event with Event="Subscription Cancelled".
func (m *StripeSchemaMapper) mapSubscriptionDeleted(event cloudsources.Event) ([]cloudsources.SegmentEvent, error) {
	return []cloudsources.SegmentEvent{
		{
			Type:              "track",
			MessageID:         uuid.New().String(),
			UserID:            event.UserID,
			Event:             "Subscription Cancelled",
			Properties:        event.Data,
			Context:           buildContext(),
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
			OriginalTimestamp: event.Timestamp.UTC().Format(time.RFC3339),
		},
	}, nil
}

// mapDefaultEvent handles unknown Stripe event types by creating a track
// event with the raw Stripe event name.
func (m *StripeSchemaMapper) mapDefaultEvent(event cloudsources.Event) ([]cloudsources.SegmentEvent, error) {
	return []cloudsources.SegmentEvent{
		{
			Type:              "track",
			MessageID:         uuid.New().String(),
			UserID:            event.UserID,
			Event:             event.Name,
			Properties:        event.Data,
			Context:           buildContext(),
			Timestamp:         time.Now().UTC().Format(time.RFC3339),
			OriginalTimestamp: event.Timestamp.UTC().Format(time.RFC3339),
		},
	}, nil
}

// mapStripeEventToSegmentType maps a Stripe event type string to a Segment
// Spec event type ("track" or "identify").
func mapStripeEventToSegmentType(stripeEvent string) string {
	if stripeEvent == eventCustomerCreated {
		return "identify"
	}
	return "track"
}

// extractCustomerID extracts the customer ID from a Stripe event data object.
// For charge, invoice, and subscription events, it checks the "customer" field.
// For customer events (where the object IS the customer), it falls back to "id".
func extractCustomerID(data map[string]interface{}) string {
	if data == nil {
		return ""
	}
	if customer, ok := data["customer"].(string); ok && customer != "" {
		return customer
	}
	if id, ok := data["id"].(string); ok && id != "" {
		return id
	}
	return ""
}

// buildContext constructs the standard Segment context object with library
// metadata and source type identification for the Stripe connector.
func buildContext() map[string]interface{} {
	return map[string]interface{}{
		"library": map[string]interface{}{
			"name":    libraryName,
			"version": libraryVersion,
		},
		"source": map[string]interface{}{
			"type": ConnectorName,
		},
	}
}

// parseStripeSignature parses the Stripe-Signature header into its
// timestamp and v1 signature components. The header format is:
// t={timestamp},v1={signature}[,v1={signature}...]
func parseStripeSignature(header string) (string, []string, error) {
	var timestamp string
	var signatures []string

	pairs := strings.Split(header, ",")
	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "t":
			timestamp = parts[1]
		case "v1":
			signatures = append(signatures, parts[1])
		}
	}

	if timestamp == "" || len(signatures) == 0 {
		return "", nil, fmt.Errorf("malformed %s header", stripeSignatureHeader)
	}
	return timestamp, signatures, nil
}

// init registers the Stripe connector with the default cloud source registry
// so it can be discovered and instantiated at runtime by the framework.
func init() {
	cloudsources.DefaultRegistry.Register(ConnectorName, func(cfg cloudsources.CloudSourceConfig) (cloudsources.CloudSource, error) {
		return NewStripeConnector(cfg)
	})
}
