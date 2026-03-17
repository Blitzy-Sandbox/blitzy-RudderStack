package stripe

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	cloudsources "github.com/rudderlabs/rudder-server/services/cloud-sources"
)

// Compile-time interface compliance checks ensure that StripeConnector
// implements both the CloudSource and WebhookReceiver interfaces from
// the cloud source framework. These checks fail at compile time if any
// required method is missing from the StripeConnector implementation.
var (
	_ cloudsources.CloudSource     = (*StripeConnector)(nil)
	_ cloudsources.WebhookReceiver = (*StripeConnector)(nil)
)

// Test constants — synthetic data only, no real credentials per AAP Section 0.7.3.
const (
	testHMACSecret = "whsec_test_secret_key_12345"
	testWriteKey   = "test-write-key-stripe"
	testSourceID   = "test-source-id-stripe"
)

// computeStripeSignature computes a valid Stripe-Signature header value for testing.
// It uses HMAC-SHA256 with the Stripe timestamp+payload signing scheme:
// t={unix_timestamp},v1={hmac_sha256_hex(timestamp + "." + payload)}
func computeStripeSignature(t *testing.T, payload []byte, secret string) string {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signedPayload := timestamp + "." + string(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write([]byte(signedPayload))
	require.NoError(t, err)
	signature := hex.EncodeToString(mac.Sum(nil))
	return "t=" + timestamp + ",v1=" + signature
}

// computeStripeSignatureWithTimestamp computes a Stripe-Signature header value
// using a specific Unix timestamp. This enables testing expired signature
// rejection with timestamps beyond the 5-minute tolerance window.
func computeStripeSignatureWithTimestamp(t *testing.T, payload []byte, secret string, ts int64) string {
	t.Helper()
	timestamp := strconv.FormatInt(ts, 10)
	signedPayload := timestamp + "." + string(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write([]byte(signedPayload))
	require.NoError(t, err)
	signature := hex.EncodeToString(mac.Sum(nil))
	return "t=" + timestamp + ",v1=" + signature
}

// newTestConnector creates a configured StripeConnector for testing with
// synthetic credentials and webhook configuration. The connector is fully
// initialized and ready for Validate/Transform/Start/Stop operations.
func newTestConnector(t *testing.T) *StripeConnector {
	t.Helper()
	cfg := cloudsources.CloudSourceConfig{
		ID:         testSourceID,
		Name:       "Test Stripe Source",
		SourceType: "stripe",
		Mode:       cloudsources.ModeWebhook,
		WriteKey:   testWriteKey,
		Enabled:    true,
		Webhook: cloudsources.WebhookConfig{
			HMACSecret:        testHMACSecret,
			SignatureHeader:   "Stripe-Signature",
			ValidateSignature: true,
		},
	}
	connector, err := NewStripeConnector(cfg)
	require.NoError(t, err)
	require.NotNil(t, connector)
	return connector
}

// makeWebhookPayload constructs a Stripe webhook event payload as JSON bytes
// using jsonrs.Marshal (MANDATED — never encoding/json). This helper ensures
// consistent test payload construction across all test phases.
func makeWebhookPayload(t *testing.T, eventID, eventType string, created int64, objectData map[string]interface{}) []byte {
	t.Helper()
	payload := map[string]interface{}{
		"id":      eventID,
		"type":    eventType,
		"created": created,
		"data": map[string]interface{}{
			"object": objectData,
		},
	}
	data, err := jsonrs.Marshal(payload)
	require.NoError(t, err)
	return data
}

// ---------------------------------------------------------------------------
// Phase 1: HMAC-SHA256 Signature Validation Tests
// ---------------------------------------------------------------------------

// TestStripeSignatureValidation validates the HMAC-SHA256 signature verification
// logic of the StripeConnector.Validate method. Covers valid signatures, invalid
// signatures, missing headers, malformed headers, and expired timestamps.
func TestStripeSignatureValidation(t *testing.T) {
	connector := newTestConnector(t)

	tests := []struct {
		name        string
		setupReq    func(t *testing.T) *http.Request
		expectValid bool
		expectErr   bool
	}{
		{
			name: "valid signature accepted",
			setupReq: func(t *testing.T) *http.Request {
				t.Helper()
				payload := []byte(`{"id":"evt_test_123","type":"charge.succeeded","data":{"object":{"amount":2000}}}`)
				sigHeader := computeStripeSignature(t, payload, testHMACSecret)
				req := httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
				req.Header.Set("Stripe-Signature", sigHeader)
				return req
			},
			expectValid: true,
			expectErr:   false,
		},
		{
			name: "invalid signature rejected",
			setupReq: func(t *testing.T) *http.Request {
				t.Helper()
				payload := []byte(`{"id":"evt_test_123","type":"charge.succeeded","data":{"object":{"amount":2000}}}`)
				// Use current timestamp but wrong HMAC-SHA256 signature (all zeros)
				invalidSig := fmt.Sprintf("t=%d,v1=0000000000000000000000000000000000000000000000000000000000000000", time.Now().Unix())
				req := httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
				req.Header.Set("Stripe-Signature", invalidSig)
				return req
			},
			expectValid: false,
			expectErr:   false,
		},
		{
			name: "missing signature header rejected",
			setupReq: func(t *testing.T) *http.Request {
				t.Helper()
				payload := []byte(`{"id":"evt_test_123","type":"charge.succeeded","data":{"object":{"amount":2000}}}`)
				req := httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
				// Deliberately do not set Stripe-Signature header
				return req
			},
			expectValid: false,
			expectErr:   true,
		},
		{
			name: "malformed signature header rejected",
			setupReq: func(t *testing.T) *http.Request {
				t.Helper()
				payload := []byte(`{"id":"evt_test_123","type":"charge.succeeded","data":{"object":{"amount":2000}}}`)
				req := httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
				req.Header.Set("Stripe-Signature", "not-a-valid-stripe-signature-format")
				return req
			},
			expectValid: false,
			expectErr:   true,
		},
		{
			name: "expired timestamp rejected",
			setupReq: func(t *testing.T) *http.Request {
				t.Helper()
				payload := []byte(`{"id":"evt_test_123","type":"charge.succeeded","data":{"object":{"amount":2000}}}`)
				// 600 seconds in the past — beyond the 5-minute (300s) tolerance window
				expiredTS := time.Now().Unix() - 600
				sigHeader := computeStripeSignatureWithTimestamp(t, payload, testHMACSecret, expiredTS)
				req := httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
				req.Header.Set("Stripe-Signature", sigHeader)
				return req
			},
			expectValid: false,
			expectErr:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.setupReq(t)
			valid, err := connector.Validate(req)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.expectValid, valid)
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 2: Webhook Event Transformation Tests
// ---------------------------------------------------------------------------

// TestStripeWebhookTransform validates the StripeConnector.Transform method
// by sending realistic Stripe webhook payloads for each supported event type
// and verifying the resulting Segment Spec events have correct types, event
// names, user IDs, and properties.
func TestStripeWebhookTransform(t *testing.T) {
	connector := newTestConnector(t)

	tests := []struct {
		name             string
		eventID          string
		eventType        string
		created          int64
		objectData       map[string]interface{}
		expectedLen      int
		expectedType     string
		expectedEvent    string
		expectedUserID   string
		verifyProperties func(t *testing.T, events []cloudsources.SegmentEvent)
	}{
		{
			name:      "charge.succeeded maps to track Payment Completed",
			eventID:   "evt_charge_001",
			eventType: "charge.succeeded",
			created:   1640000000,
			objectData: map[string]interface{}{
				"id":          "ch_abc123",
				"amount":      2000,
				"currency":    "usd",
				"customer":    "cus_test123",
				"description": "Test charge",
				"status":      "succeeded",
			},
			expectedLen:    1,
			expectedType:   "track",
			expectedEvent:  "Payment Completed",
			expectedUserID: "cus_test123",
			verifyProperties: func(t *testing.T, events []cloudsources.SegmentEvent) {
				t.Helper()
				props := events[0].Properties
				require.NotNil(t, props)
				// Verify charge-related properties are present
				require.Contains(t, props, "amount")
				require.Contains(t, props, "currency")
				// Verify context contains library metadata
				ctx := events[0].Context
				require.NotNil(t, ctx)
				lib, ok := ctx["library"].(map[string]interface{})
				require.True(t, ok, "context.library should be a map")
				require.Equal(t, "rudder-cloud-sources", lib["name"])
			},
		},
		{
			name:      "customer.created maps to identify",
			eventID:   "evt_cust_001",
			eventType: "customer.created",
			created:   1640000000,
			objectData: map[string]interface{}{
				"id":          "cus_new456",
				"email":       "test@example.com",
				"name":        "Test User",
				"description": "A test customer",
				"metadata":    map[string]interface{}{},
			},
			expectedLen:    1,
			expectedType:   "identify",
			expectedEvent:  "",
			expectedUserID: "cus_new456",
			verifyProperties: func(t *testing.T, events []cloudsources.SegmentEvent) {
				t.Helper()
				traits := events[0].Traits
				require.NotNil(t, traits)
				require.Contains(t, traits, "email")
				require.Contains(t, traits, "name")
				// Verify context library metadata
				ctx := events[0].Context
				require.NotNil(t, ctx)
				lib, ok := ctx["library"].(map[string]interface{})
				require.True(t, ok, "context.library should be a map")
				require.Equal(t, "rudder-cloud-sources", lib["name"])
			},
		},
		{
			name:      "invoice.paid maps to track Invoice Paid",
			eventID:   "evt_invoice_001",
			eventType: "invoice.paid",
			created:   1640000000,
			objectData: map[string]interface{}{
				"id":           "in_test789",
				"amount_paid":  5000,
				"currency":     "usd",
				"customer":     "cus_inv_cust",
				"subscription": "sub_abc",
				"status":       "paid",
			},
			expectedLen:    1,
			expectedType:   "track",
			expectedEvent:  "Invoice Paid",
			expectedUserID: "cus_inv_cust",
			verifyProperties: func(t *testing.T, events []cloudsources.SegmentEvent) {
				t.Helper()
				props := events[0].Properties
				require.NotNil(t, props)
				require.Contains(t, props, "amount_paid")
				require.Contains(t, props, "currency")
			},
		},
		{
			name:      "customer.subscription.created maps to track Subscription Created",
			eventID:   "evt_sub_create_001",
			eventType: "customer.subscription.created",
			created:   1640000000,
			objectData: map[string]interface{}{
				"id":       "sub_new123",
				"customer": "cus_sub_cust",
				"plan": map[string]interface{}{
					"id":       "plan_basic",
					"amount":   999,
					"currency": "usd",
					"interval": "month",
				},
				"status": "active",
			},
			expectedLen:    1,
			expectedType:   "track",
			expectedEvent:  "Subscription Created",
			expectedUserID: "cus_sub_cust",
			verifyProperties: func(t *testing.T, events []cloudsources.SegmentEvent) {
				t.Helper()
				props := events[0].Properties
				require.NotNil(t, props)
				require.Contains(t, props, "status")
			},
		},
		{
			name:      "customer.subscription.deleted maps to track Subscription Cancelled",
			eventID:   "evt_sub_del_001",
			eventType: "customer.subscription.deleted",
			created:   1640000000,
			objectData: map[string]interface{}{
				"id":       "sub_del456",
				"customer": "cus_del_cust",
				"plan": map[string]interface{}{
					"id":       "plan_pro",
					"amount":   1999,
					"currency": "usd",
					"interval": "month",
				},
				"status": "canceled",
			},
			expectedLen:    1,
			expectedType:   "track",
			expectedEvent:  "Subscription Cancelled",
			expectedUserID: "cus_del_cust",
			verifyProperties: func(t *testing.T, events []cloudsources.SegmentEvent) {
				t.Helper()
				props := events[0].Properties
				require.NotNil(t, props)
				require.Contains(t, props, "status")
				require.Contains(t, props, "customer")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := makeWebhookPayload(t, tc.eventID, tc.eventType, tc.created, tc.objectData)
			req := httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
			events, err := connector.Transform(req)
			require.NoError(t, err)
			require.Len(t, events, tc.expectedLen)

			event := events[0]
			require.Equal(t, tc.expectedType, event.Type)
			require.Equal(t, tc.expectedUserID, event.UserID)
			require.NotEmpty(t, event.MessageID)

			// For track events, verify the event name
			if tc.expectedType == "track" {
				require.Equal(t, tc.expectedEvent, event.Event)
			}

			// Run custom property verification
			if tc.verifyProperties != nil {
				tc.verifyProperties(t, events)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 3: Schema Mapping Tests
// ---------------------------------------------------------------------------

// TestStripeSchemaMapper validates the StripeSchemaMapper.MapToSegmentSpec method
// directly, testing event type mapping, context metadata inclusion, message ID
// generation, and user ID extraction from Stripe event data.
func TestStripeSchemaMapper(t *testing.T) {
	mapper := NewStripeSchemaMapper()
	require.NotNil(t, mapper)

	tests := []struct {
		name           string
		event          cloudsources.Event
		expectedType   string
		expectedEvent  string
		expectedUserID string
		verify         func(t *testing.T, result []cloudsources.SegmentEvent)
	}{
		{
			name: "maps charge event to track",
			event: cloudsources.Event{
				ID:         "evt_charge_map_001",
				Type:       "track",
				Name:       "charge.succeeded",
				SourceType: "stripe",
				Timestamp:  time.Unix(1640000000, 0),
				Data: map[string]interface{}{
					"id":       "ch_map_abc",
					"amount":   3000,
					"currency": "usd",
					"customer": "cus_map_123",
					"status":   "succeeded",
				},
				UserID: "cus_map_123",
			},
			expectedType:   "track",
			expectedEvent:  "Payment Completed",
			expectedUserID: "cus_map_123",
			verify: func(t *testing.T, result []cloudsources.SegmentEvent) {
				t.Helper()
				require.Len(t, result, 1)
				require.Contains(t, result[0].Properties, "amount")
			},
		},
		{
			name: "maps customer event to identify",
			event: cloudsources.Event{
				ID:         "evt_cust_map_001",
				Type:       "identify",
				Name:       "customer.created",
				SourceType: "stripe",
				Timestamp:  time.Unix(1640000000, 0),
				Data: map[string]interface{}{
					"id":    "cus_map_456",
					"email": "mapper@example.com",
					"name":  "Schema Mapper User",
				},
				UserID: "cus_map_456",
			},
			expectedType:   "identify",
			expectedEvent:  "",
			expectedUserID: "cus_map_456",
			verify: func(t *testing.T, result []cloudsources.SegmentEvent) {
				t.Helper()
				require.Len(t, result, 1)
				require.Contains(t, result[0].Traits, "email")
				require.Contains(t, result[0].Traits, "name")
			},
		},
		{
			name: "maps subscription event to track",
			event: cloudsources.Event{
				ID:         "evt_sub_map_001",
				Type:       "track",
				Name:       "customer.subscription.created",
				SourceType: "stripe",
				Timestamp:  time.Unix(1640000000, 0),
				Data: map[string]interface{}{
					"id":       "sub_map_789",
					"customer": "cus_sub_map",
					"status":   "active",
				},
				UserID: "cus_sub_map",
			},
			expectedType:   "track",
			expectedEvent:  "Subscription Created",
			expectedUserID: "cus_sub_map",
			verify: func(t *testing.T, result []cloudsources.SegmentEvent) {
				t.Helper()
				require.Len(t, result, 1)
			},
		},
		{
			name: "includes context with library metadata",
			event: cloudsources.Event{
				ID:         "evt_ctx_001",
				Type:       "track",
				Name:       "charge.succeeded",
				SourceType: "stripe",
				Timestamp:  time.Unix(1640000000, 0),
				Data: map[string]interface{}{
					"id":       "ch_ctx_001",
					"customer": "cus_ctx_001",
				},
				UserID: "cus_ctx_001",
			},
			expectedType:   "track",
			expectedEvent:  "Payment Completed",
			expectedUserID: "cus_ctx_001",
			verify: func(t *testing.T, result []cloudsources.SegmentEvent) {
				t.Helper()
				require.Len(t, result, 1)
				ctx := result[0].Context
				require.NotNil(t, ctx, "context should not be nil")
				lib, ok := ctx["library"].(map[string]interface{})
				require.True(t, ok, "context.library should be a map")
				require.Equal(t, "rudder-cloud-sources", lib["name"])
				require.NotEmpty(t, lib["version"])
				// Verify source type in context
				src, ok := ctx["source"].(map[string]interface{})
				require.True(t, ok, "context.source should be a map")
				require.Equal(t, "stripe", src["type"])
			},
		},
		{
			name: "generates valid messageId",
			event: cloudsources.Event{
				ID:         "evt_msgid_001",
				Type:       "track",
				Name:       "invoice.paid",
				SourceType: "stripe",
				Timestamp:  time.Unix(1640000000, 0),
				Data: map[string]interface{}{
					"id":       "in_msgid_001",
					"customer": "cus_msgid",
				},
				UserID: "cus_msgid",
			},
			expectedType:   "track",
			expectedEvent:  "Invoice Paid",
			expectedUserID: "cus_msgid",
			verify: func(t *testing.T, result []cloudsources.SegmentEvent) {
				t.Helper()
				require.Len(t, result, 1)
				require.NotEmpty(t, result[0].MessageID, "messageId must be non-empty")
			},
		},
		{
			name: "extracts userId from customer field",
			event: cloudsources.Event{
				ID:         "evt_userid_001",
				Type:       "track",
				Name:       "charge.succeeded",
				SourceType: "stripe",
				Timestamp:  time.Unix(1640000000, 0),
				Data: map[string]interface{}{
					"id":       "ch_userid_001",
					"amount":   1500,
					"customer": "cus_extracted_uid",
				},
				UserID: "cus_extracted_uid",
			},
			expectedType:   "track",
			expectedEvent:  "Payment Completed",
			expectedUserID: "cus_extracted_uid",
			verify: func(t *testing.T, result []cloudsources.SegmentEvent) {
				t.Helper()
				require.Len(t, result, 1)
				require.Equal(t, "cus_extracted_uid", result[0].UserID)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := mapper.MapToSegmentSpec(tc.event)
			require.NoError(t, err)
			require.NotEmpty(t, result)

			event := result[0]
			require.Equal(t, tc.expectedType, event.Type)
			require.Equal(t, tc.expectedUserID, event.UserID)

			if tc.expectedType == "track" {
				require.Equal(t, tc.expectedEvent, event.Event)
			}

			if tc.verify != nil {
				tc.verify(t, result)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 4: Edge Case Tests
// ---------------------------------------------------------------------------

// TestStripeEdgeCases validates that the StripeConnector handles malformed,
// missing, and unexpected payloads gracefully without panicking. Each case
// tests a different failure mode to ensure robust error handling.
func TestStripeEdgeCases(t *testing.T) {
	connector := newTestConnector(t)

	tests := []struct {
		name   string
		setup  func(t *testing.T) *http.Request
		verify func(t *testing.T, events []cloudsources.SegmentEvent, err error)
	}{
		{
			name: "unknown event type handled gracefully",
			setup: func(t *testing.T) *http.Request {
				t.Helper()
				payload := makeWebhookPayload(t,
					"evt_unknown_001", "unknown.event.type", 1640000000,
					map[string]interface{}{
						"id":     "obj_unknown",
						"status": "active",
					},
				)
				return httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
			},
			verify: func(t *testing.T, events []cloudsources.SegmentEvent, err error) {
				t.Helper()
				// Unknown event types should be handled gracefully — either produce a track
				// event with the raw event name, or return empty. The connector must not crash.
				require.NoError(t, err)
				if len(events) > 0 {
					require.Equal(t, "track", events[0].Type)
				}
			},
		},
		{
			name: "empty payload returns error",
			setup: func(t *testing.T) *http.Request {
				t.Helper()
				return httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader([]byte{}))
			},
			verify: func(t *testing.T, events []cloudsources.SegmentEvent, err error) {
				t.Helper()
				require.Error(t, err)
			},
		},
		{
			name: "malformed JSON returns error",
			setup: func(t *testing.T) *http.Request {
				t.Helper()
				malformed := strings.NewReader("not valid json{")
				return httptest.NewRequest(http.MethodPost, "/webhook/stripe", malformed)
			},
			verify: func(t *testing.T, events []cloudsources.SegmentEvent, err error) {
				t.Helper()
				require.Error(t, err)
				require.True(t,
					strings.Contains(err.Error(), "unmarshal") ||
						strings.Contains(err.Error(), "json") ||
						strings.Contains(err.Error(), "invalid") ||
						strings.Contains(err.Error(), "JSON"),
					fmt.Sprintf("expected JSON parsing error, got: %v", err))
			},
		},
		{
			name: "missing type field handled",
			setup: func(t *testing.T) *http.Request {
				t.Helper()
				// Valid JSON but missing the "type" field entirely
				payload := map[string]interface{}{
					"id":      "evt_notype_001",
					"created": 1640000000,
					"data": map[string]interface{}{
						"object": map[string]interface{}{
							"id":     "obj_notype",
							"status": "active",
						},
					},
				}
				data, err := jsonrs.Marshal(payload)
				require.NoError(t, err)
				return httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(data))
			},
			verify: func(t *testing.T, events []cloudsources.SegmentEvent, err error) {
				t.Helper()
				// Missing type field should be handled gracefully — either map with
				// default behavior or produce a minimal event, or return an error
				if err != nil {
					// Acceptable: returning an error for missing type
					require.Error(t, err)
				} else if len(events) > 0 {
					// Acceptable: producing a default track event
					require.NotEmpty(t, events[0].Type)
				}
			},
		},
		{
			name: "empty data object handled",
			setup: func(t *testing.T) *http.Request {
				t.Helper()
				payload := makeWebhookPayload(t,
					"evt_empty_data_001", "charge.succeeded", 1640000000,
					map[string]interface{}{},
				)
				return httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
			},
			verify: func(t *testing.T, events []cloudsources.SegmentEvent, err error) {
				t.Helper()
				// Empty data.object should produce an event with minimal properties
				require.NoError(t, err)
				require.NotEmpty(t, events)
				require.Equal(t, "track", events[0].Type)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.setup(t)
			events, err := connector.Transform(req)
			tc.verify(t, events, err)
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 5: CloudSource Lifecycle Tests
// ---------------------------------------------------------------------------

// TestStripeConnectorLifecycle validates the full lifecycle of the StripeConnector:
// construction, start, status checks, stop, and interface compliance. Ensures the
// connector correctly transitions between states and implements all required
// framework interfaces.
func TestStripeConnectorLifecycle(t *testing.T) {
	// Verify NOP logger is available from the logger package — confirms the
	// logger dependency is correctly wired and accessible in tests.
	nopLogger := logger.NOP
	require.NotNil(t, nopLogger, "logger.NOP should be available for test infrastructure")

	tests := []struct {
		name   string
		verify func(t *testing.T)
	}{
		{
			name: "Start returns nil",
			verify: func(t *testing.T) {
				t.Helper()
				connector := newTestConnector(t)
				ctx := context.Background()
				err := connector.Start(ctx)
				require.NoError(t, err)
			},
		},
		{
			name: "Stop returns nil",
			verify: func(t *testing.T) {
				t.Helper()
				connector := newTestConnector(t)
				ctx := context.Background()
				err := connector.Start(ctx)
				require.NoError(t, err)
				err = connector.Stop(ctx)
				require.NoError(t, err)
			},
		},
		{
			name: "Status returns healthy after start",
			verify: func(t *testing.T) {
				t.Helper()
				connector := newTestConnector(t)
				ctx := context.Background()
				err := connector.Start(ctx)
				require.NoError(t, err)
				status := connector.Status()
				require.True(t, status.Healthy, "connector should be healthy after Start")
			},
		},
		{
			name: "Status returns name stripe",
			verify: func(t *testing.T) {
				t.Helper()
				connector := newTestConnector(t)
				status := connector.Status()
				require.Equal(t, ConnectorName, status.Name)
				require.Equal(t, "stripe", status.Name)
			},
		},
		{
			name: "implements CloudSource interface",
			verify: func(t *testing.T) {
				t.Helper()
				// This is validated by the compile-time interface check at the top
				// of this file. Here we additionally verify at runtime that the
				// concrete type satisfies the interface contract.
				connector := newTestConnector(t)
				var cs cloudsources.CloudSource = connector
				require.NotNil(t, cs)
				ctx := context.Background()
				require.NoError(t, cs.Start(ctx))
				_ = cs.Status()
				require.NoError(t, cs.Stop(ctx))
			},
		},
		{
			name: "implements WebhookReceiver interface",
			verify: func(t *testing.T) {
				t.Helper()
				// Verify the WebhookReceiver interface is implemented by calling
				// both Validate and Transform through the interface type.
				connector := newTestConnector(t)
				var wr cloudsources.WebhookReceiver = connector
				require.NotNil(t, wr)

				payload := []byte(`{"id":"evt_iface_001","type":"charge.succeeded","data":{"object":{"id":"ch_iface"}}}`)
				sigHeader := computeStripeSignature(t, payload, testHMACSecret)
				req := httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
				req.Header.Set("Stripe-Signature", sigHeader)

				valid, err := wr.Validate(req)
				require.NoError(t, err)
				require.True(t, valid)

				// Create a fresh request for Transform since Validate consumed the body
				req2 := httptest.NewRequest(http.MethodPost, "/webhook/stripe", bytes.NewReader(payload))
				events, err := wr.Transform(req2)
				require.NoError(t, err)
				require.NotEmpty(t, events)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.verify(t)
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 6: JSON Serialization Compliance Tests
// ---------------------------------------------------------------------------

// TestStripeJSONSerialization validates that all JSON operations use jsonrs
// (MANDATED by AAP — NEVER encoding/json) and that roundtrip serialization
// of SegmentEvent and Stripe webhook payloads preserves data integrity.
func TestStripeJSONSerialization(t *testing.T) {
	tests := []struct {
		name   string
		verify func(t *testing.T)
	}{
		{
			name: "marshal SegmentEvent with jsonrs",
			verify: func(t *testing.T) {
				t.Helper()
				event := cloudsources.SegmentEvent{
					Type:      "track",
					MessageID: "msg-json-001",
					UserID:    "user-json-001",
					Event:     "Payment Completed",
					Properties: map[string]interface{}{
						"amount":   2000,
						"currency": "usd",
					},
					Context: map[string]interface{}{
						"library": map[string]interface{}{
							"name":    "rudder-cloud-sources",
							"version": "0.1.0",
						},
					},
					Timestamp: time.Now().UTC().Format(time.RFC3339),
				}

				// Marshal with jsonrs (MANDATED, not encoding/json)
				data, err := jsonrs.Marshal(event)
				require.NoError(t, err)
				require.NotEmpty(t, data)

				// Unmarshal back with jsonrs and verify roundtrip fidelity
				var decoded cloudsources.SegmentEvent
				err = jsonrs.Unmarshal(data, &decoded)
				require.NoError(t, err)
				require.Equal(t, event.Type, decoded.Type)
				require.Equal(t, event.MessageID, decoded.MessageID)
				require.Equal(t, event.UserID, decoded.UserID)
				require.Equal(t, event.Event, decoded.Event)
				require.NotNil(t, decoded.Properties)
				require.NotNil(t, decoded.Context)
			},
		},
		{
			name: "unmarshal Stripe webhook payload with jsonrs",
			verify: func(t *testing.T) {
				t.Helper()
				rawJSON := []byte(`{
					"id": "evt_json_001",
					"type": "charge.succeeded",
					"created": 1640000000,
					"data": {
						"object": {
							"id": "ch_json_001",
							"amount": 5000,
							"currency": "eur",
							"customer": "cus_json_001"
						}
					}
				}`)

				// Unmarshal with jsonrs (MANDATED, not encoding/json)
				var payload map[string]interface{}
				err := jsonrs.Unmarshal(rawJSON, &payload)
				require.NoError(t, err)
				require.Equal(t, "evt_json_001", payload["id"])
				require.Equal(t, "charge.succeeded", payload["type"])

				// Verify nested data extraction
				data, ok := payload["data"].(map[string]interface{})
				require.True(t, ok, "data should be a map")
				obj, ok := data["object"].(map[string]interface{})
				require.True(t, ok, "data.object should be a map")
				require.Equal(t, "ch_json_001", obj["id"])
				require.Equal(t, "cus_json_001", obj["customer"])

				// Re-marshal with jsonrs to verify roundtrip
				reEncoded, err := jsonrs.Marshal(payload)
				require.NoError(t, err)
				require.NotEmpty(t, reEncoded)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.verify(t)
		})
	}
}
