package validator

import (
	"errors"
	"testing"
	"time"

	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-schemas/go/stream"
)

func TestMessageIDValidator(t *testing.T) {
	t.Run("valid message ID", func(t *testing.T) {
		v := newMessageIDValidator()
		payload := []byte(`{"messageId": "test-msg-id"}`)

		valid, err := v.Validate(payload, nil)
		require.NoError(t, err)
		require.True(t, valid)
	})

	t.Run("missing message ID", func(t *testing.T) {
		v := newMessageIDValidator()
		payload := []byte(`{}`)

		valid, err := v.Validate(payload, nil)
		require.NoError(t, err)
		require.False(t, valid)
	})
}

func TestReqTypeValidator(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		props   *stream.MessageProperties // Add props to test cases
		valid   bool
	}{
		{"valid type", `{"type":"track"}`, &stream.MessageProperties{RequestType: "track"}, true},
		{"invalid type", `{}`, &stream.MessageProperties{RequestType: "invalid"}, false},
		{"missing type", `{}`, &stream.MessageProperties{}, true}, // Empty properties
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newReqTypeValidator()
			valid, err := v.Validate([]byte(tt.payload), tt.props) // Pass props
			require.NoError(t, err)
			require.Equal(t, tt.valid, valid)
		})
	}
}

func TestReceivedAtValidator(t *testing.T) {
	now := time.Now().Format(time.RFC3339Nano)

	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{"valid timestamp", `{"receivedAt": "` + now + `"}`, true},
		{"missing field", `{}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newReceivedAtValidator()
			valid, err := v.Validate([]byte(tt.payload), nil)
			require.NoError(t, err)
			require.Equal(t, tt.valid, valid)
		})
	}
}

func TestRudderIDValidator(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{"valid user ID", `{"rudderId": "user123"}`, true},
		{"missing both IDs", `{}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newRudderIDValidator()
			valid, err := v.Validate([]byte(tt.payload), nil)
			require.NoError(t, err)
			require.Equal(t, tt.valid, valid)
		})
	}
}

func TestMediator_Validate(t *testing.T) {
	validPayload := []byte(`{
        "messageId": "msg123",
        "type": "track",
        "receivedAt": "2023-10-01T12:00:00Z",
        "userId": "user123",
		"rudderId": "rud-id",
		"request_ip": "192.168.1.1"
    }`)

	baseProps := &stream.MessageProperties{
		RequestType: "track",
		RequestIP:   "192.168.1.1",
		RoutingKey:  "rand-routeing-key",
		WorkspaceID: "workspace-id",
		SourceID:    "source-id",
		ReceivedAt:  time.Now(),
	}

	tests := []struct {
		name          string
		mockValidator func(*stream.MessageProperties) error
		payload       []byte
		props         *stream.MessageProperties
		want          bool
		wantErr       bool
	}{
		{
			name:          "all validations pass",
			payload:       validPayload,
			props:         baseProps,
			mockValidator: func(*stream.MessageProperties) error { return nil },
			want:          true,
			wantErr:       false,
		},
		{
			name:          "msg properties validation fails",
			payload:       validPayload,
			props:         baseProps,
			mockValidator: func(*stream.MessageProperties) error { return errors.New("validation error") },
			want:          false,
			wantErr:       true,
		},
		{
			name:          "missing message ID",
			payload:       []byte(`{"type": "track"}`),
			props:         baseProps,
			mockValidator: func(*stream.MessageProperties) error { return nil },
			want:          false,
			wantErr:       false,
		},
		{
			name:          "invalid receivedAt format",
			payload:       []byte(`{"receivedAt": "invalid"}`),
			props:         baseProps,
			mockValidator: func(*stream.MessageProperties) error { return nil },
			want:          false,
			wantErr:       false,
		},
		{
			name:          "missing user identifiers",
			payload:       []byte(`{"messageId": "msg123"}`),
			props:         baseProps,
			mockValidator: func(*stream.MessageProperties) error { return nil },
			want:          false,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mediator := NewValidateMediator(logger.NOP, tt.mockValidator)
			got, err := mediator.Validate(tt.payload, tt.props)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.True(t, got == tt.want, "expected %v, got %v", tt.want, got)
		})
	}
}

// TestValidatorWithClientHintsPayload verifies that the validator mediator chain
// does NOT reject payloads containing structured Client Hints data in context.userAgentData.
// This is the core assertion for ES-001 (Structured Client Hints Pass-Through Verification):
// since all validators only inspect top-level fields (messageId, type, receivedAt, request_ip,
// rudderId) via gjson, the nested context.userAgentData object must pass through without
// interference. The negative test case confirms that normal validation still applies when
// Client Hints are present but required fields are missing.
func TestValidatorWithClientHintsPayload(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		props       *stream.MessageProperties
		validatorFn func(*stream.MessageProperties) error
		wantValid   bool
		wantErr     bool
	}{
		{
			name: "payload with Client Hints userAgentData passes all validators",
			payload: `{
				"messageId": "msg-ch-001",
				"type": "track",
				"receivedAt": "2024-06-15T10:30:00Z",
				"request_ip": "203.0.113.50",
				"rudderId": "rudder-ch-001",
				"context": {
					"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
					"userAgentData": {
						"brands": [
							{"brand": "Chromium", "version": "110"},
							{"brand": "Google Chrome", "version": "110"}
						],
						"mobile": false,
						"platform": "macOS",
						"bitness": "64",
						"platformVersion": "13.1.0"
					}
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "track",
				RequestIP:   "203.0.113.50",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-id",
				SourceID:    "source-id",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "identify payload with Client Hints and all standard context fields passes",
			payload: `{
				"messageId": "msg-ch-002",
				"type": "identify",
				"userId": "user-001",
				"receivedAt": "2024-06-15T10:30:00Z",
				"request_ip": "203.0.113.51",
				"rudderId": "rudder-ch-002",
				"traits": {"email": "test@example.com"},
				"context": {
					"userAgent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
					"userAgentData": {
						"brands": [
							{"brand": "Chromium", "version": "110"},
							{"brand": "Google Chrome", "version": "110"},
							{"brand": "Not?A_Brand", "version": "24"}
						],
						"mobile": false,
						"platform": "Windows",
						"fullVersionList": [
							{"brand": "Chromium", "version": "110.0.5481.178"},
							{"brand": "Google Chrome", "version": "110.0.5481.178"}
						],
						"wow64": false
					},
					"ip": "203.0.113.51",
					"library": {"name": "analytics.js", "version": "2.1.0"},
					"channel": "browser"
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "identify",
				RequestIP:   "203.0.113.51",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-id",
				SourceID:    "source-id",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "mobile Client Hints payload passes all validators",
			payload: `{
				"messageId": "msg-ch-003",
				"type": "track",
				"event": "Mobile Event",
				"receivedAt": "2024-06-15T10:30:00Z",
				"request_ip": "203.0.113.52",
				"rudderId": "rudder-ch-003",
				"context": {
					"userAgentData": {
						"brands": [
							{"brand": "Chromium", "version": "110"}
						],
						"mobile": true,
						"platform": "Android",
						"model": "Pixel 7",
						"platformVersion": "13.0.0"
					}
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "track",
				RequestIP:   "203.0.113.52",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-id",
				SourceID:    "source-id",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "empty Client Hints object passes all validators",
			payload: `{
				"messageId": "msg-ch-004",
				"type": "page",
				"name": "Home",
				"receivedAt": "2024-06-15T10:30:00Z",
				"request_ip": "203.0.113.53",
				"rudderId": "rudder-ch-004",
				"context": {
					"userAgentData": {}
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "page",
				RequestIP:   "203.0.113.53",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-id",
				SourceID:    "source-id",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "Client Hints payload still fails when required fields missing",
			payload: `{
				"type": "track",
				"receivedAt": "2024-06-15T10:30:00Z",
				"request_ip": "203.0.113.54",
				"rudderId": "rudder-ch-005",
				"context": {
					"userAgentData": {
						"brands": [{"brand": "Chrome", "version": "110"}],
						"mobile": false,
						"platform": "macOS"
					}
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "track",
				RequestIP:   "203.0.113.54",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-id",
				SourceID:    "source-id",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   false,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mediator := NewValidateMediator(logger.NOP, tt.validatorFn)
			valid, err := mediator.Validate([]byte(tt.payload), tt.props)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.wantValid, valid,
				"Validation result mismatch for test: %s", tt.name)
		})
	}
}

// TestSDKPayloadValidation verifies that all Segment SDK payload formats pass the
// validator mediator chain without rejection. The validators only inspect top-level
// gateway-injected fields (messageId, type, receivedAt, request_ip, rudderId) and
// a msgProperties callback. SDK-specific nested context fields (context.device,
// context.os, context.app, context.network, context.screen, context.library,
// context.page, _metadata, sentAt) must NOT cause any validator to reject.
func TestSDKPayloadValidation(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		props       *stream.MessageProperties
		validatorFn func(*stream.MessageProperties) error
		wantValid   bool
		wantErr     bool
	}{
		{
			name: "SDK payload with mobile-specific context fields passes validation",
			payload: `{
				"messageId": "msg-mobile-001",
				"type": "track",
				"event": "Product Viewed",
				"userId": "mobile-user-001",
				"anonymousId": "anon-mobile-001",
				"receivedAt": "2024-09-15T10:00:00Z",
				"request_ip": "203.0.113.10",
				"rudderId": "rudder-mobile-001",
				"context": {
					"device": {
						"id": "device-001",
						"manufacturer": "Apple",
						"model": "iPhone 15",
						"name": "Test iPhone",
						"type": "ios"
					},
					"os": {"name": "iOS", "version": "17.2"},
					"app": {"name": "TestApp", "version": "2.1.0", "build": "1234", "namespace": "com.test.app"},
					"network": {"bluetooth": false, "carrier": "T-Mobile", "cellular": true, "wifi": true},
					"screen": {"density": 3, "height": 2532, "width": 1170},
					"library": {"name": "analytics-ios", "version": "4.1.0"}
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "track",
				RequestIP:   "203.0.113.10",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-sdk",
				SourceID:    "source-sdk",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "Server-side batch payload passes validation",
			payload: `{
				"messageId": "msg-server-001",
				"type": "track",
				"event": "Order Completed",
				"userId": "server-user-001",
				"receivedAt": "2024-09-15T10:01:00Z",
				"request_ip": "203.0.113.20",
				"rudderId": "rudder-server-001",
				"context": {
					"library": {"name": "analytics-node", "version": "6.2.0"},
					"channel": "server"
				},
				"properties": {
					"orderId": "order-123",
					"revenue": 99.99,
					"currency": "USD"
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "track",
				RequestIP:   "203.0.113.20",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-sdk",
				SourceID:    "source-sdk",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "Beacon payload passes validation",
			payload: `{
				"messageId": "msg-beacon-001",
				"type": "track",
				"event": "Page View",
				"anonymousId": "anon-beacon-001",
				"receivedAt": "2024-09-15T10:02:00Z",
				"request_ip": "203.0.113.30",
				"rudderId": "rudder-beacon-001",
				"context": {
					"library": {"name": "analytics.js", "version": "2.0.0"},
					"channel": "client",
					"page": {
						"url": "https://example.com/products",
						"path": "/products",
						"title": "Products",
						"referrer": "https://example.com/"
					},
					"userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "track",
				RequestIP:   "203.0.113.30",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-sdk",
				SourceID:    "source-sdk",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "iOS SDK identify payload passes validation",
			payload: `{
				"messageId": "msg-ios-identify-001",
				"type": "identify",
				"userId": "ios-user-001",
				"anonymousId": "anon-ios-001",
				"receivedAt": "2024-09-15T10:03:00Z",
				"request_ip": "203.0.113.40",
				"rudderId": "rudder-ios-001",
				"traits": {
					"name": "Jane Doe",
					"email": "jane.doe@example.com",
					"plan": "premium"
				},
				"context": {
					"library": {"name": "analytics-ios", "version": "4.1.0"},
					"device": {"id": "ios-device-002", "manufacturer": "Apple", "model": "iPad Pro", "name": "Test iPad", "type": "ios"},
					"os": {"name": "iPadOS", "version": "17.1"},
					"app": {"name": "TestApp", "version": "3.0.0", "build": "5678", "namespace": "com.test.ipadapp"},
					"network": {"bluetooth": true, "carrier": "AT&T", "cellular": false, "wifi": true},
					"screen": {"density": 2, "height": 2732, "width": 2048},
					"channel": "client"
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "identify",
				RequestIP:   "203.0.113.40",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-sdk",
				SourceID:    "source-sdk",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "Android SDK track payload passes validation",
			payload: `{
				"messageId": "msg-android-track-001",
				"type": "track",
				"event": "Item Added to Cart",
				"userId": "android-user-001",
				"anonymousId": "anon-android-001",
				"receivedAt": "2024-09-15T10:04:00Z",
				"request_ip": "203.0.113.50",
				"rudderId": "rudder-android-001",
				"properties": {
					"productId": "prod-789",
					"quantity": 2,
					"price": 29.99
				},
				"context": {
					"library": {"name": "analytics-android", "version": "4.11.3"},
					"device": {"id": "android-device-001", "manufacturer": "Samsung", "model": "Galaxy S24", "name": "Test Galaxy", "type": "android"},
					"os": {"name": "Android", "version": "14"},
					"app": {"name": "TestApp", "version": "4.0.0", "build": "9012", "namespace": "com.test.androidapp"},
					"network": {"bluetooth": true, "carrier": "Verizon", "cellular": true, "wifi": false},
					"screen": {"density": 2.75, "height": 2340, "width": 1080},
					"channel": "client"
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "track",
				RequestIP:   "203.0.113.50",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-sdk",
				SourceID:    "source-sdk",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "analytics.js page payload passes validation",
			payload: `{
				"messageId": "msg-js-page-001",
				"type": "page",
				"name": "Pricing",
				"anonymousId": "anon-js-001",
				"receivedAt": "2024-09-15T10:05:00Z",
				"request_ip": "203.0.113.60",
				"rudderId": "rudder-js-001",
				"properties": {
					"url": "https://example.com/pricing",
					"path": "/pricing",
					"title": "Pricing - Example",
					"referrer": "https://example.com/",
					"search": "?plan=enterprise"
				},
				"context": {
					"library": {"name": "analytics.js", "version": "1.68.0"},
					"page": {
						"url": "https://example.com/pricing",
						"path": "/pricing",
						"title": "Pricing - Example",
						"referrer": "https://example.com/",
						"search": "?plan=enterprise"
					},
					"userAgent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
					"channel": "client"
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "page",
				RequestIP:   "203.0.113.60",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-sdk",
				SourceID:    "source-sdk",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "Server-side SDK payload with sentAt field passes validation",
			payload: `{
				"messageId": "msg-python-001",
				"type": "identify",
				"userId": "python-user-001",
				"receivedAt": "2024-09-15T10:06:00Z",
				"request_ip": "203.0.113.70",
				"rudderId": "rudder-python-001",
				"sentAt": "2024-09-15T10:05:58Z",
				"traits": {
					"email": "user@example.com",
					"company": "Acme Corp"
				},
				"context": {
					"library": {"name": "analytics-python", "version": "2.2.3"},
					"channel": "server"
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "identify",
				RequestIP:   "203.0.113.70",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-sdk",
				SourceID:    "source-sdk",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
		{
			name: "Analytics 2.0 payload with _metadata field passes validation",
			payload: `{
				"messageId": "msg-a2-001",
				"type": "track",
				"event": "Experiment Viewed",
				"anonymousId": "anon-a2-001",
				"receivedAt": "2024-09-15T10:07:00Z",
				"request_ip": "203.0.113.80",
				"rudderId": "rudder-a2-001",
				"properties": {
					"experimentId": "exp-123",
					"variationId": "var-A"
				},
				"context": {
					"library": {"name": "analytics.js", "version": "2.1.0"},
					"channel": "client"
				},
				"_metadata": {
					"bundled": ["Amplitude", "Google Analytics"],
					"unbundled": ["Mixpanel"],
					"bundledIds": ["amp-123", "ga-456"]
				}
			}`,
			props: &stream.MessageProperties{
				RequestType: "track",
				RequestIP:   "203.0.113.80",
				RoutingKey:  "test-routing-key",
				WorkspaceID: "workspace-sdk",
				SourceID:    "source-sdk",
				ReceivedAt:  time.Now(),
			},
			validatorFn: func(*stream.MessageProperties) error { return nil },
			wantValid:   true,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mediator := NewValidateMediator(logger.NOP, tt.validatorFn)
			valid, err := mediator.Validate([]byte(tt.payload), tt.props)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.wantValid, valid,
				"Validation result mismatch for test: %s", tt.name)
		})
	}
}
