package cloudsources

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
)

// ---------------------------------------------------------------------------
// Mock implementations for interface compliance tests
// ---------------------------------------------------------------------------

// mockCloudSource is a minimal CloudSource implementation for testing the
// interface contract. It records calls and returns configurable results.
type mockCloudSource struct {
	startErr error
	stopErr  error
	status   SourceStatus
}

func (m *mockCloudSource) Start(_ context.Context) error { return m.startErr }
func (m *mockCloudSource) Stop(_ context.Context) error  { return m.stopErr }
func (m *mockCloudSource) Status() SourceStatus          { return m.status }

// mockPoller is a minimal Poller implementation for testing the interface contract.
type mockPoller struct {
	events []Event
	err    error
	cursor string
}

func (m *mockPoller) Poll(_ context.Context) ([]Event, error) { return m.events, m.err }
func (m *mockPoller) SetCursor(cursor string)                 { m.cursor = cursor }
func (m *mockPoller) GetCursor() string                       { return m.cursor }

// mockWebhookReceiver is a minimal WebhookReceiver implementation for testing.
type mockWebhookReceiver struct {
	validateResult bool
	validateErr    error
	transformResult []SegmentEvent
	transformErr   error
}

func (m *mockWebhookReceiver) Validate(_ *http.Request) (bool, error) {
	return m.validateResult, m.validateErr
}
func (m *mockWebhookReceiver) Transform(_ *http.Request) ([]SegmentEvent, error) {
	return m.transformResult, m.transformErr
}

// mockSchemaMapper is a minimal SchemaMapper implementation for testing.
type mockSchemaMapper struct {
	events []SegmentEvent
	err    error
}

func (m *mockSchemaMapper) MapToSegmentSpec(_ Event) ([]SegmentEvent, error) {
	return m.events, m.err
}

// ---------------------------------------------------------------------------
// Phase 1: Interface Compliance Tests
// ---------------------------------------------------------------------------

// TestCloudSourceInterface verifies that a struct implementing the CloudSource
// interface can be created and its lifecycle methods (Start, Stop, Status)
// behave according to the interface contract.
func TestCloudSourceInterface(t *testing.T) {
	tests := []struct {
		name     string
		source   CloudSource
		validate func(t *testing.T, src CloudSource)
	}{
		{
			name: "Start returns nil for valid config",
			source: &mockCloudSource{
				startErr: nil,
				status:   SourceStatus{Name: "test-source", Healthy: true},
			},
			validate: func(t *testing.T, src CloudSource) {
				err := src.Start(context.Background())
				require.NoError(t, err)
			},
		},
		{
			name: "Stop returns nil",
			source: &mockCloudSource{
				stopErr: nil,
				status:  SourceStatus{Name: "test-source", Healthy: true},
			},
			validate: func(t *testing.T, src CloudSource) {
				err := src.Stop(context.Background())
				require.NoError(t, err)
			},
		},
		{
			name: "Status returns valid SourceStatus",
			source: &mockCloudSource{
				status: SourceStatus{
					Name:           "stripe-webhook",
					Healthy:        true,
					Message:        "all systems operational",
					EventsIngested: 42,
				},
			},
			validate: func(t *testing.T, src CloudSource) {
				status := src.Status()
				require.Equal(t, "stripe-webhook", status.Name)
				require.True(t, status.Healthy)
				require.Equal(t, "all systems operational", status.Message)
				require.Equal(t, int64(42), status.EventsIngested)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.validate(t, tc.source)
		})
	}
}

// TestPollerInterface verifies the Poller interface contract: Poll returns
// events, SetCursor/GetCursor manage cursor state, and a new poller has
// an empty cursor.
func TestPollerInterface(t *testing.T) {
	tests := []struct {
		name     string
		setup    func() Poller
		validate func(t *testing.T, p Poller)
	}{
		{
			name: "Poll returns events",
			setup: func() Poller {
				return &mockPoller{
					events: []Event{
						{ID: "evt-001", Type: "track", Name: "order.completed", SourceType: "stripe"},
						{ID: "evt-002", Type: "identify", SourceType: "stripe"},
					},
				}
			},
			validate: func(t *testing.T, p Poller) {
				events, err := p.Poll(context.Background())
				require.NoError(t, err)
				require.Len(t, events, 2)
				require.Equal(t, "evt-001", events[0].ID)
				require.Equal(t, "track", events[0].Type)
				require.Equal(t, "evt-002", events[1].ID)
			},
		},
		{
			name: "SetCursor stores cursor",
			setup: func() Poller {
				return &mockPoller{}
			},
			validate: func(t *testing.T, p Poller) {
				p.SetCursor("cursor-abc-123")
				cursor := p.GetCursor()
				require.Equal(t, "cursor-abc-123", cursor)
			},
		},
		{
			name: "GetCursor returns empty string initially",
			setup: func() Poller {
				return &mockPoller{}
			},
			validate: func(t *testing.T, p Poller) {
				cursor := p.GetCursor()
				require.Equal(t, "", cursor)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.setup()
			tc.validate(t, p)
		})
	}
}

// TestWebhookReceiverInterface verifies the WebhookReceiver interface
// contract: Validate accepts/rejects signatures, Transform produces SegmentEvents.
func TestWebhookReceiverInterface(t *testing.T) {
	tests := []struct {
		name     string
		setup    func() WebhookReceiver
		validate func(t *testing.T, wr WebhookReceiver)
	}{
		{
			name: "Validate accepts valid signature",
			setup: func() WebhookReceiver {
				return &mockWebhookReceiver{validateResult: true}
			},
			validate: func(t *testing.T, wr WebhookReceiver) {
				req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{}`)))
				valid, err := wr.Validate(req)
				require.NoError(t, err)
				require.True(t, valid)
			},
		},
		{
			name: "Validate rejects invalid signature",
			setup: func() WebhookReceiver {
				return &mockWebhookReceiver{validateResult: false}
			},
			validate: func(t *testing.T, wr WebhookReceiver) {
				req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{}`)))
				valid, err := wr.Validate(req)
				require.NoError(t, err)
				require.False(t, valid)
			},
		},
		{
			name: "Transform produces SegmentEvents",
			setup: func() WebhookReceiver {
				return &mockWebhookReceiver{
					transformResult: []SegmentEvent{
						{Type: "track", Event: "charge.succeeded", MessageID: "msg-001"},
					},
				}
			},
			validate: func(t *testing.T, wr WebhookReceiver) {
				payload := []byte(`{"type":"charge.succeeded"}`)
				req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))
				events, err := wr.Transform(req)
				require.NoError(t, err)
				require.Len(t, events, 1)
				require.Equal(t, "track", events[0].Type)
				require.Equal(t, "charge.succeeded", events[0].Event)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wr := tc.setup()
			tc.validate(t, wr)
		})
	}
}

// TestSchemaMapperInterface verifies the SchemaMapper interface contract:
// MapToSegmentSpec correctly routes track, identify, and group events.
func TestSchemaMapperInterface(t *testing.T) {
	tests := []struct {
		name       string
		mapper     SchemaMapper
		inputEvent Event
		validate   func(t *testing.T, events []SegmentEvent, err error)
	}{
		{
			name: "MapToSegmentSpec converts track event",
			mapper: &mockSchemaMapper{
				events: []SegmentEvent{
					{Type: "track", Event: "order.completed", Properties: map[string]interface{}{"amount": float64(99.99)}},
				},
			},
			inputEvent: Event{Type: "track", Name: "order.completed"},
			validate: func(t *testing.T, events []SegmentEvent, err error) {
				require.NoError(t, err)
				require.Len(t, events, 1)
				require.Equal(t, "track", events[0].Type)
				require.Equal(t, "order.completed", events[0].Event)
			},
		},
		{
			name: "MapToSegmentSpec converts identify event",
			mapper: &mockSchemaMapper{
				events: []SegmentEvent{
					{Type: "identify", Traits: map[string]interface{}{"email": "user@test.example"}},
				},
			},
			inputEvent: Event{Type: "identify"},
			validate: func(t *testing.T, events []SegmentEvent, err error) {
				require.NoError(t, err)
				require.Len(t, events, 1)
				require.Equal(t, "identify", events[0].Type)
				require.NotNil(t, events[0].Traits)
			},
		},
		{
			name: "MapToSegmentSpec converts group event",
			mapper: &mockSchemaMapper{
				events: []SegmentEvent{
					{Type: "group", GroupID: "grp-42", Traits: map[string]interface{}{"plan": "enterprise"}},
				},
			},
			inputEvent: Event{Type: "group"},
			validate: func(t *testing.T, events []SegmentEvent, err error) {
				require.NoError(t, err)
				require.Len(t, events, 1)
				require.Equal(t, "group", events[0].Type)
				require.Equal(t, "grp-42", events[0].GroupID)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := tc.mapper.MapToSegmentSpec(tc.inputEvent)
			tc.validate(t, events, err)
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 2: Registry Tests
// ---------------------------------------------------------------------------

// TestRegistry verifies the connector registry's Register, Get, List, and Len
// methods, including overwrite behavior and concurrent access safety.
func TestRegistry(t *testing.T) {
	t.Run("Register and Get connector", func(t *testing.T) {
		reg := NewRegistry()
		factory := ConnectorFactory(func(cfg CloudSourceConfig) (CloudSource, error) {
			return &mockCloudSource{status: SourceStatus{Name: "stripe"}}, nil
		})
		reg.Register("stripe", factory)

		got, ok := reg.Get("stripe")
		require.True(t, ok)
		require.NotNil(t, got)

		// Verify the factory creates the expected connector
		src, err := got(CloudSourceConfig{})
		require.NoError(t, err)
		require.Equal(t, "stripe", src.Status().Name)
	})

	t.Run("Get unknown connector returns nil", func(t *testing.T) {
		reg := NewRegistry()
		got, ok := reg.Get("nonexistent")
		require.False(t, ok)
		require.Nil(t, got)
	})

	t.Run("List returns all registered connectors", func(t *testing.T) {
		reg := NewRegistry()
		for _, name := range []string{"stripe", "hubspot", "salesforce"} {
			localName := name
			reg.Register(localName, func(cfg CloudSourceConfig) (CloudSource, error) {
				return &mockCloudSource{status: SourceStatus{Name: localName}}, nil
			})
		}

		names := reg.List()
		require.Len(t, names, 3)
		// List returns sorted names
		require.Equal(t, []string{"hubspot", "salesforce", "stripe"}, names)
	})

	t.Run("Register overwrites existing", func(t *testing.T) {
		reg := NewRegistry()

		// Register v1 factory
		reg.Register("stripe", func(cfg CloudSourceConfig) (CloudSource, error) {
			return &mockCloudSource{status: SourceStatus{Name: "stripe-v1"}}, nil
		})

		// Overwrite with v2 factory
		reg.Register("stripe", func(cfg CloudSourceConfig) (CloudSource, error) {
			return &mockCloudSource{status: SourceStatus{Name: "stripe-v2"}}, nil
		})

		got, ok := reg.Get("stripe")
		require.True(t, ok)
		require.NotNil(t, got)

		src, err := got(CloudSourceConfig{})
		require.NoError(t, err)
		require.Equal(t, "stripe-v2", src.Status().Name)
	})

	t.Run("Concurrent access is safe", func(t *testing.T) {
		reg := NewRegistry()
		var wg sync.WaitGroup
		const goroutines = 50

		// Launch goroutines doing concurrent Register, Get, and List
		wg.Add(goroutines * 3)

		for i := 0; i < goroutines; i++ {
			localI := i

			// Writer goroutine: Register
			go func() {
				defer wg.Done()
				name := "connector-" + time.Duration(localI).String()
				reg.Register(name, func(cfg CloudSourceConfig) (CloudSource, error) {
					return &mockCloudSource{}, nil
				})
			}()

			// Reader goroutine: Get
			go func() {
				defer wg.Done()
				name := "connector-" + time.Duration(localI).String()
				_, _ = reg.Get(name)
			}()

			// Reader goroutine: List
			go func() {
				defer wg.Done()
				_ = reg.List()
			}()
		}

		// If this completes without a data race panic, the RWMutex protection is working
		wg.Wait()
		require.True(t, reg.Len() > 0, "registry should contain at least some connectors")
	})

	t.Run("Len returns correct count", func(t *testing.T) {
		reg := NewRegistry()
		require.Equal(t, 0, reg.Len())

		reg.Register("a", func(cfg CloudSourceConfig) (CloudSource, error) { return &mockCloudSource{}, nil })
		require.Equal(t, 1, reg.Len())

		reg.Register("b", func(cfg CloudSourceConfig) (CloudSource, error) { return &mockCloudSource{}, nil })
		require.Equal(t, 2, reg.Len())
	})

	t.Run("Get is case-insensitive", func(t *testing.T) {
		reg := NewRegistry()
		reg.Register("Stripe", func(cfg CloudSourceConfig) (CloudSource, error) {
			return &mockCloudSource{status: SourceStatus{Name: "stripe"}}, nil
		})

		// Get with lowercase should find it
		got, ok := reg.Get("stripe")
		require.True(t, ok)
		require.NotNil(t, got)

		// Get with uppercase should also find it
		got2, ok2 := reg.Get("STRIPE")
		require.True(t, ok2)
		require.NotNil(t, got2)
	})
}

// ---------------------------------------------------------------------------
// Phase 3: Configuration Tests
// ---------------------------------------------------------------------------

// TestCloudSourceConfig verifies configuration type construction and field
// accessibility for all config structs: PollingConfig, WebhookConfig,
// CredentialConfig, and CloudSourceConfig.
func TestCloudSourceConfig(t *testing.T) {
	t.Run("Valid polling config", func(t *testing.T) {
		cfg := PollingConfig{
			Interval:      5 * time.Minute,
			MaxRetries:    3,
			RateLimit:     60,
			InitialCursor: "",
			PageSize:      100,
			Timeout:       30 * time.Second,
		}
		require.Equal(t, 5*time.Minute, cfg.Interval)
		require.Equal(t, 3, cfg.MaxRetries)
		require.Equal(t, 60, cfg.RateLimit)
		require.Equal(t, "", cfg.InitialCursor)
		require.Equal(t, 100, cfg.PageSize)
		require.Equal(t, 30*time.Second, cfg.Timeout)
	})

	t.Run("Valid webhook config", func(t *testing.T) {
		cfg := WebhookConfig{
			URL:               "https://gateway.example.com/v1/webhook",
			HMACSecret:        "whsec_test_secret_synthetic_only",
			SignatureHeader:    "X-Webhook-Signature",
			ValidateSignature: true,
		}
		require.Equal(t, "https://gateway.example.com/v1/webhook", cfg.URL)
		require.Equal(t, "whsec_test_secret_synthetic_only", cfg.HMACSecret)
		require.Equal(t, "X-Webhook-Signature", cfg.SignatureHeader)
		require.True(t, cfg.ValidateSignature)
	})

	t.Run("Valid credential config", func(t *testing.T) {
		cfg := CredentialConfig{
			APIKey:       "enc_ak_test_synthetic_only",
			APISecret:    "enc_as_test_synthetic_only",
			AccessToken:  "enc_at_test_synthetic_only",
			RefreshToken: "enc_rt_test_synthetic_only",
			IsEncrypted:  true,
		}
		require.Equal(t, "enc_ak_test_synthetic_only", cfg.APIKey)
		require.Equal(t, "enc_as_test_synthetic_only", cfg.APISecret)
		require.Equal(t, "enc_at_test_synthetic_only", cfg.AccessToken)
		require.Equal(t, "enc_rt_test_synthetic_only", cfg.RefreshToken)
		require.True(t, cfg.IsEncrypted)
	})

	t.Run("CloudSourceConfig with polling mode", func(t *testing.T) {
		cfg := CloudSourceConfig{
			ID:          "src-001",
			Name:        "Salesforce Sync",
			SourceType:  "salesforce",
			Mode:        ModePolling,
			WorkspaceID: "ws-test-001",
			WriteKey:    "wk_test_synthetic_only",
			Enabled:     true,
			Credentials: CredentialConfig{
				AccessToken: "enc_oauth_token_synthetic",
				IsEncrypted: true,
			},
			Polling: PollingConfig{
				Interval:   10 * time.Minute,
				MaxRetries: 5,
				RateLimit:  30,
				PageSize:   200,
				Timeout:    60 * time.Second,
			},
		}
		require.Equal(t, ModePolling, cfg.Mode)
		require.Equal(t, "salesforce", cfg.SourceType)
		require.True(t, cfg.Enabled)
		require.Equal(t, 10*time.Minute, cfg.Polling.Interval)
		require.Equal(t, 5, cfg.Polling.MaxRetries)
		require.True(t, cfg.Credentials.IsEncrypted)
	})

	t.Run("CloudSourceConfig with webhook mode", func(t *testing.T) {
		cfg := CloudSourceConfig{
			ID:          "src-002",
			Name:        "Stripe Webhooks",
			SourceType:  "stripe",
			Mode:        ModeWebhook,
			WorkspaceID: "ws-test-001",
			WriteKey:    "wk_test_synthetic_only",
			Enabled:     true,
			Credentials: CredentialConfig{
				APIKey:      "enc_stripe_key_synthetic",
				IsEncrypted: true,
			},
			Webhook: WebhookConfig{
				URL:               "https://gateway.example.com/v1/webhook/stripe",
				HMACSecret:        "whsec_test_secret_synthetic",
				SignatureHeader:    "Stripe-Signature",
				ValidateSignature: true,
			},
		}
		require.Equal(t, ModeWebhook, cfg.Mode)
		require.Equal(t, "stripe", cfg.SourceType)
		require.True(t, cfg.Webhook.ValidateSignature)
		require.Equal(t, "Stripe-Signature", cfg.Webhook.SignatureHeader)
	})

	t.Run("Default polling config has correct defaults", func(t *testing.T) {
		cfg := NewDefaultPollingConfig()
		require.Equal(t, DefaultPollingInterval, cfg.Interval)
		require.Equal(t, DefaultMaxRetries, cfg.MaxRetries)
		require.Equal(t, DefaultRateLimit, cfg.RateLimit)
		require.Equal(t, DefaultPageSize, cfg.PageSize)
		require.Equal(t, DefaultTimeout, cfg.Timeout)
		require.Equal(t, "", cfg.InitialCursor)
	})

	t.Run("Default webhook config has correct defaults", func(t *testing.T) {
		cfg := NewDefaultWebhookConfig()
		require.True(t, cfg.ValidateSignature)
		require.Equal(t, "X-Webhook-Signature", cfg.SignatureHeader)
		require.Equal(t, "", cfg.URL)
		require.Equal(t, "", cfg.HMACSecret)
	})

	t.Run("Config JSON round-trip with jsonrs", func(t *testing.T) {
		original := CloudSourceConfig{
			ID:          "src-rt-001",
			Name:        "JSON Roundtrip Test",
			SourceType:  "test",
			Mode:        ModeWebhook,
			WorkspaceID: "ws-rt-001",
			WriteKey:    "wk_rt_test",
			Enabled:     true,
			Webhook: WebhookConfig{
				URL:               "https://example.com/webhook",
				SignatureHeader:    "X-Test-Sig",
				ValidateSignature: true,
			},
		}

		data, err := jsonrs.Marshal(original)
		require.NoError(t, err)
		require.NotEmpty(t, data)

		var restored CloudSourceConfig
		err = jsonrs.Unmarshal(data, &restored)
		require.NoError(t, err)
		require.Equal(t, original.ID, restored.ID)
		require.Equal(t, original.Name, restored.Name)
		require.Equal(t, original.SourceType, restored.SourceType)
		require.Equal(t, original.Mode, restored.Mode)
		require.Equal(t, original.Enabled, restored.Enabled)
		require.Equal(t, original.Webhook.URL, restored.Webhook.URL)
		require.Equal(t, original.Webhook.SignatureHeader, restored.Webhook.SignatureHeader)
		require.Equal(t, original.Webhook.ValidateSignature, restored.Webhook.ValidateSignature)
	})
}

// ---------------------------------------------------------------------------
// Phase 4: Poller Tests
// ---------------------------------------------------------------------------

// TestBasePoller verifies the BasePoller implementation: context cancellation,
// cursor round-trip, and polling interval behavior.
func TestBasePoller(t *testing.T) {
	t.Run("Poll with context cancellation", func(t *testing.T) {
		callCount := 0
		pollFn := PollFunc(func(ctx context.Context, cursor string) ([]Event, string, error) {
			callCount++
			return []Event{{ID: "evt-poll-1", Type: "track", Name: "test.event"}}, "cursor-1", nil
		})

		poller := NewBasePoller(PollingConfig{
			Interval: 50 * time.Millisecond,
		}, pollFn, logger.NOP)

		ctx, cancel := context.WithCancel(context.Background())

		// Start poller in background goroutine
		done := make(chan error, 1)
		go func() {
			done <- poller.Start(ctx)
		}()

		// Wait for at least one poll cycle
		time.Sleep(100 * time.Millisecond)

		// Cancel context to stop polling
		cancel()

		// Wait for Start to return
		err := <-done
		require.ErrorIs(t, err, context.Canceled)

		// Verify at least one poll executed
		require.True(t, callCount >= 1, "expected at least one poll cycle, got %d", callCount)
	})

	t.Run("SetCursor and GetCursor roundtrip", func(t *testing.T) {
		pollFn := PollFunc(func(_ context.Context, cursor string) ([]Event, string, error) {
			return nil, "", nil
		})

		poller := NewBasePoller(PollingConfig{
			Interval: time.Minute,
		}, pollFn, logger.NOP)

		// Initial cursor should be empty
		require.Equal(t, "", poller.GetCursor())

		// Set and retrieve cursor
		poller.SetCursor("abc123")
		require.Equal(t, "abc123", poller.GetCursor())

		// Update cursor again
		poller.SetCursor("def456")
		require.Equal(t, "def456", poller.GetCursor())
	})

	t.Run("Poll respects interval", func(t *testing.T) {
		pollCount := 0
		pollFn := PollFunc(func(_ context.Context, cursor string) ([]Event, string, error) {
			pollCount++
			return []Event{{ID: "evt-interval", Type: "track"}}, "next", nil
		})

		interval := 50 * time.Millisecond
		poller := NewBasePoller(PollingConfig{
			Interval: interval,
		}, pollFn, logger.NOP)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- poller.Start(ctx)
		}()

		// Wait for ~3 intervals (initial + 2 ticks = ~3 polls)
		time.Sleep(160 * time.Millisecond)
		cancel()
		<-done

		// Should have polled approximately 3-4 times (initial + 2-3 ticks)
		// The exact count depends on goroutine scheduling, but should be ≥2
		require.True(t, pollCount >= 2, "expected at least 2 polls, got %d", pollCount)
	})

	t.Run("Poll updates cursor on success", func(t *testing.T) {
		pollFn := PollFunc(func(_ context.Context, cursor string) ([]Event, string, error) {
			return []Event{{ID: "evt-cursor"}}, "new-cursor-" + cursor, nil
		})

		poller := NewBasePoller(PollingConfig{
			Interval:      time.Minute,
			InitialCursor: "start",
		}, pollFn, logger.NOP)

		require.Equal(t, "start", poller.GetCursor())

		events, err := poller.Poll(context.Background())
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, "new-cursor-start", poller.GetCursor())
	})

	t.Run("Poll does not update cursor on error", func(t *testing.T) {
		pollFn := PollFunc(func(_ context.Context, _ string) ([]Event, string, error) {
			return nil, "should-not-be-set", errPollFailed
		})

		poller := NewBasePoller(PollingConfig{
			Interval:      time.Minute,
			InitialCursor: "original",
		}, pollFn, logger.NOP)

		events, err := poller.Poll(context.Background())
		require.Error(t, err)
		require.Nil(t, events)
		require.Equal(t, "original", poller.GetCursor(), "cursor should not change on error")
	})

	t.Run("Poll returns context error on cancelled context", func(t *testing.T) {
		pollFn := PollFunc(func(_ context.Context, _ string) ([]Event, string, error) {
			return nil, "", nil
		})

		poller := NewBasePoller(PollingConfig{
			Interval: time.Minute,
		}, pollFn, logger.NOP)

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		events, err := poller.Poll(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, events)
	})

	t.Run("IsRunning reflects polling state", func(t *testing.T) {
		pollFn := PollFunc(func(_ context.Context, _ string) ([]Event, string, error) {
			return nil, "", nil
		})

		poller := NewBasePoller(PollingConfig{
			Interval: 50 * time.Millisecond,
		}, pollFn, logger.NOP)

		require.False(t, poller.IsRunning())

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- poller.Start(ctx)
		}()

		// Wait briefly for Start to set running=true
		time.Sleep(20 * time.Millisecond)
		require.True(t, poller.IsRunning())

		cancel()
		<-done

		// After cancellation, running should be false
		require.False(t, poller.IsRunning())
	})

	t.Run("Default interval applied when zero", func(t *testing.T) {
		pollFn := PollFunc(func(_ context.Context, _ string) ([]Event, string, error) {
			return nil, "", nil
		})

		poller := NewBasePoller(PollingConfig{
			Interval: 0, // zero should be replaced with default
		}, pollFn, logger.NOP)

		require.Equal(t, DefaultPollingInterval, poller.Config.Interval)
	})

	t.Run("Events delivered through channel", func(t *testing.T) {
		pollFn := PollFunc(func(_ context.Context, _ string) ([]Event, string, error) {
			return []Event{
				{ID: "ch-evt-1", Type: "track", Name: "channel.test"},
			}, "next", nil
		})

		poller := NewBasePoller(PollingConfig{
			Interval: 100 * time.Millisecond,
		}, pollFn, logger.NOP)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- poller.Start(ctx)
		}()

		// Read the first batch from the Events channel
		select {
		case events := <-poller.Events:
			require.Len(t, events, 1)
			require.Equal(t, "ch-evt-1", events[0].ID)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for events on channel")
		}

		cancel()
		<-done
	})
}

// errPollFailed is a sentinel error for poll failure test scenarios.
var errPollFailed = errSentinel("poll failed")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// Phase 5: Webhook Receiver Tests
// ---------------------------------------------------------------------------

// computeHMACSignature is a test helper that computes an HMAC-SHA256 hex
// signature of the given payload using the provided secret.
func computeHMACSignature(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestBaseWebhookReceiver verifies the BaseWebhookReceiver implementation:
// HMAC signature validation, payload transformation, and replay protection.
func TestBaseWebhookReceiver(t *testing.T) {
	const testSecret = "whsec_test_secret_for_hmac_validation"

	t.Run("ValidateSignature with valid HMAC-SHA256", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			HMACSecret:        testSecret,
			SignatureHeader:    "X-Webhook-Signature",
			ValidateSignature: true,
		}, mapper, logger.NOP)

		payload := []byte(`{"id":"evt-sig-001","type":"charge.succeeded","amount":1000}`)
		signature := computeHMACSignature(payload, testSecret)

		result := receiver.ValidateSignature(payload, signature)
		require.True(t, result)
	})

	t.Run("ValidateSignature with invalid signature", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			HMACSecret:        testSecret,
			SignatureHeader:    "X-Webhook-Signature",
			ValidateSignature: true,
		}, mapper, logger.NOP)

		payload := []byte(`{"id":"evt-sig-002","type":"charge.failed"}`)
		wrongSignature := computeHMACSignature(payload, "wrong_secret_entirely")

		result := receiver.ValidateSignature(payload, wrongSignature)
		require.False(t, result)
	})

	t.Run("ValidateSignature with empty signature", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			HMACSecret:        testSecret,
			SignatureHeader:    "X-Webhook-Signature",
			ValidateSignature: true,
		}, mapper, logger.NOP)

		payload := []byte(`{"id":"evt-sig-003","type":"customer.created"}`)

		result := receiver.ValidateSignature(payload, "")
		require.False(t, result)
	})

	t.Run("Validate accepts request with valid HMAC header", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			HMACSecret:        testSecret,
			SignatureHeader:    "X-Webhook-Signature",
			ValidateSignature: true,
		}, mapper, logger.NOP)

		payload := []byte(`{"id":"evt-val-001","type":"invoice.paid","amount":5000}`)
		signature := computeHMACSignature(payload, testSecret)

		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))
		req.Header.Set("X-Webhook-Signature", signature)

		valid, err := receiver.Validate(req)
		require.NoError(t, err)
		require.True(t, valid)
	})

	t.Run("Validate rejects request with missing signature header", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			HMACSecret:        testSecret,
			SignatureHeader:    "X-Webhook-Signature",
			ValidateSignature: true,
		}, mapper, logger.NOP)

		payload := []byte(`{"id":"evt-val-002","type":"charge.refunded"}`)
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))
		// No signature header set

		valid, err := receiver.Validate(req)
		require.NoError(t, err)
		require.False(t, valid)
	})

	t.Run("Validate skips when no HMAC secret configured", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			HMACSecret:        "", // No secret = skip validation
			SignatureHeader:    "X-Webhook-Signature",
			ValidateSignature: true,
		}, mapper, logger.NOP)

		payload := []byte(`{"id":"evt-val-003","type":"payment.created"}`)
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payload))

		valid, err := receiver.Validate(req)
		require.NoError(t, err)
		require.True(t, valid, "should pass when no secret is configured")
	})

	t.Run("Transform normalizes payload", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("stripe")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			SignatureHeader: "X-Webhook-Signature",
		}, mapper, logger.NOP)

		payload := map[string]interface{}{
			"id":          "evt-transform-001",
			"type":        "charge.succeeded",
			"amount":      float64(2500),
			"currency":    "usd",
			"customer_id": "cus_test_synthetic_001",
		}

		payloadBytes, err := jsonrs.Marshal(payload)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payloadBytes))
		req.Header.Set("Content-Type", "application/json")

		events, err := receiver.Transform(req)
		require.NoError(t, err)
		require.Len(t, events, 1)

		evt := events[0]
		require.Equal(t, "track", evt.Type, "charge.succeeded should map to track event")
		require.NotEmpty(t, evt.MessageID)
		require.NotEmpty(t, evt.Timestamp)
		require.NotNil(t, evt.Context)

		// Verify context contains library info
		lib, ok := evt.Context["library"].(map[string]interface{})
		require.True(t, ok, "context.library should be a map")
		require.Equal(t, "rudder-cloud-sources", lib["name"])
		require.Equal(t, "0.1.0", lib["version"])
	})

	t.Run("Replay protection rejects duplicate", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("stripe")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			SignatureHeader: "X-Webhook-Signature",
		}, mapper, logger.NOP)

		payload := map[string]interface{}{
			"id":   "evt-replay-001",
			"type": "payment.created",
			"amount": float64(500),
		}
		payloadBytes, err := jsonrs.Marshal(payload)
		require.NoError(t, err)

		// First request — should produce events
		req1 := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payloadBytes))
		events1, err := receiver.Transform(req1)
		require.NoError(t, err)
		require.Len(t, events1, 1, "first request should produce events")

		// Second request with same event ID — should be flagged as replay
		req2 := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payloadBytes))
		events2, err := receiver.Transform(req2)
		require.NoError(t, err)
		require.Len(t, events2, 0, "duplicate request should produce empty events (replay protection)")
	})

	t.Run("Transform with identify event type", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("hubspot")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			SignatureHeader: "X-Webhook-Signature",
		}, mapper, logger.NOP)

		payload := map[string]interface{}{
			"id":    "evt-identify-001",
			"type":  "identify",
			"email": "jane@test.example",
			"name":  "Jane Doe",
		}
		payloadBytes, err := jsonrs.Marshal(payload)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(payloadBytes))
		events, err := receiver.Transform(req)
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, "identify", events[0].Type)
		require.NotNil(t, events[0].Traits)
	})

	t.Run("Default signature header when empty", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")
		receiver := NewBaseWebhookReceiver(WebhookConfig{
			HMACSecret:      testSecret,
			SignatureHeader: "", // Should default to X-Webhook-Signature
		}, mapper, logger.NOP)

		require.Equal(t, "X-Webhook-Signature", receiver.SignatureHeader)
	})
}

// ---------------------------------------------------------------------------
// Phase 6: Schema Mapper Tests
// ---------------------------------------------------------------------------

// TestSchemaMapper verifies the BaseSchemaMapper implementation: event type
// routing (track, identify, group), unknown type fallback, and empty data handling.
func TestSchemaMapper(t *testing.T) {
	t.Run("Map track event", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("stripe")

		event := Event{
			ID:         "evt-map-001",
			Type:       "track",
			Name:       "charge.succeeded",
			SourceType: "stripe",
			Timestamp:  time.Now(),
			Data: map[string]interface{}{
				"amount":   float64(2500),
				"currency": "usd",
				"customer_id": "cus_synthetic_001",
			},
			UserID: "user-001",
		}

		segmentEvents, err := mapper.MapToSegmentSpec(event)
		require.NoError(t, err)
		require.Len(t, segmentEvents, 1)

		se := segmentEvents[0]
		require.Equal(t, "track", se.Type)
		require.Equal(t, "charge.succeeded", se.Event)
		require.Equal(t, "user-001", se.UserID)
		require.NotEmpty(t, se.MessageID)
		require.NotEmpty(t, se.Timestamp)
		require.NotEmpty(t, se.OriginalTimestamp)
		require.NotEmpty(t, se.SentAt)
		require.NotNil(t, se.Properties)
		require.NotNil(t, se.Context)

		// Verify context metadata
		lib, ok := se.Context["library"].(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, "rudder-cloud-sources", lib["name"])
		require.Equal(t, "0.1.0", lib["version"])

		source, ok := se.Context["source"].(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, "stripe", source["type"])

		// Verify integrations default
		require.NotNil(t, se.Integrations)
		require.Equal(t, true, se.Integrations["All"])
	})

	t.Run("Map identify event", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("hubspot")

		event := Event{
			ID:         "evt-map-002",
			Type:       "identify",
			Name:       "contact.created",
			SourceType: "hubspot",
			Timestamp:  time.Now(),
			Data: map[string]interface{}{
				"email":      "john@test.example",
				"first_name": "John",
				"last_name":  "Doe",
				"company":    "Acme Corp",
			},
			UserID: "user-002",
		}

		segmentEvents, err := mapper.MapToSegmentSpec(event)
		require.NoError(t, err)
		require.Len(t, segmentEvents, 1)

		se := segmentEvents[0]
		require.Equal(t, "identify", se.Type)
		require.Equal(t, "user-002", se.UserID)
		require.NotNil(t, se.Traits)
		require.Equal(t, "john@test.example", se.Traits["email"])
		require.Equal(t, "John", se.Traits["first_name"])
		// Properties should be empty for identify events
		require.Nil(t, se.Properties)
	})

	t.Run("Map group event", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("salesforce")

		event := Event{
			ID:         "evt-map-003",
			Type:       "group",
			Name:       "account.updated",
			SourceType: "salesforce",
			Timestamp:  time.Now(),
			Data: map[string]interface{}{
				"groupId":  "grp-synthetic-42",
				"name":     "Acme Corporation",
				"industry": "Technology",
				"plan":     "enterprise",
			},
			UserID: "user-003",
		}

		segmentEvents, err := mapper.MapToSegmentSpec(event)
		require.NoError(t, err)
		require.Len(t, segmentEvents, 1)

		se := segmentEvents[0]
		require.Equal(t, "group", se.Type)
		require.Equal(t, "grp-synthetic-42", se.GroupID)
		require.Equal(t, "user-003", se.UserID)
		require.NotNil(t, se.Traits)
		require.Equal(t, "Acme Corporation", se.Traits["name"])
	})

	t.Run("Unknown event type defaults to track", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("custom")

		event := Event{
			ID:         "evt-map-004",
			Type:       "custom_webhook_event",
			Name:       "webhook.received",
			SourceType: "custom",
			Timestamp:  time.Now(),
			Data: map[string]interface{}{
				"payload": "synthetic-data",
			},
		}

		segmentEvents, err := mapper.MapToSegmentSpec(event)
		require.NoError(t, err)
		require.Len(t, segmentEvents, 1)

		se := segmentEvents[0]
		require.Equal(t, "track", se.Type, "unknown event type should default to track")
		require.Equal(t, "webhook.received", se.Event)
		require.NotNil(t, se.Properties)
	})

	t.Run("Empty data produces minimal event", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")

		event := Event{
			ID:         "evt-map-005",
			Type:       "track",
			Name:       "empty.event",
			SourceType: "test",
			Timestamp:  time.Now(),
			Data:       map[string]interface{}{},
		}

		segmentEvents, err := mapper.MapToSegmentSpec(event)
		require.NoError(t, err)
		require.Len(t, segmentEvents, 1)

		se := segmentEvents[0]
		require.Equal(t, "track", se.Type)
		require.Equal(t, "empty.event", se.Event)
		require.NotNil(t, se.Properties)
		require.NotEmpty(t, se.MessageID)
		require.NotEmpty(t, se.Timestamp)
	})

	t.Run("Nil data produces minimal event", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")

		event := Event{
			ID:         "evt-map-006",
			Type:       "track",
			Name:       "nil.data.event",
			SourceType: "test",
			Timestamp:  time.Now(),
			Data:       nil,
		}

		segmentEvents, err := mapper.MapToSegmentSpec(event)
		require.NoError(t, err)
		require.Len(t, segmentEvents, 1)

		se := segmentEvents[0]
		require.Equal(t, "track", se.Type)
		require.NotNil(t, se.Properties)
	})

	t.Run("AnonymousID generated when no UserID", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")

		event := Event{
			ID:         "evt-map-007",
			Type:       "track",
			Name:       "anonymous.event",
			SourceType: "test",
			Timestamp:  time.Now(),
			Data:       map[string]interface{}{},
			UserID:     "", // No user ID
		}

		segmentEvents, err := mapper.MapToSegmentSpec(event)
		require.NoError(t, err)
		require.Len(t, segmentEvents, 1)

		se := segmentEvents[0]
		require.Equal(t, "", se.UserID)
		require.NotEmpty(t, se.AnonymousID, "anonymousID should be generated when no userID")
	})

	t.Run("UserID extracted from event data when not set explicitly", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")

		event := Event{
			ID:         "evt-map-008",
			Type:       "identify",
			Name:       "user.updated",
			SourceType: "test",
			Timestamp:  time.Now(),
			Data: map[string]interface{}{
				"userId": "extracted-user-123",
				"email":  "test@test.example",
			},
			UserID: "", // Not set explicitly
		}

		segmentEvents, err := mapper.MapToSegmentSpec(event)
		require.NoError(t, err)
		require.Len(t, segmentEvents, 1)

		se := segmentEvents[0]
		require.Equal(t, "extracted-user-123", se.UserID)
		require.Equal(t, "", se.AnonymousID)
	})

	t.Run("SegmentEvent JSON roundtrip with jsonrs", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("stripe")

		event := Event{
			ID:         "evt-json-rt",
			Type:       "track",
			Name:       "charge.captured",
			SourceType: "stripe",
			Timestamp:  time.Now(),
			Data: map[string]interface{}{
				"amount":   float64(5000),
				"currency": "eur",
			},
			UserID: "user-json-rt",
		}

		segmentEvents, err := mapper.MapToSegmentSpec(event)
		require.NoError(t, err)
		require.Len(t, segmentEvents, 1)

		// Marshal to JSON using jsonrs
		data, err := jsonrs.Marshal(segmentEvents[0])
		require.NoError(t, err)
		require.NotEmpty(t, data)

		// Unmarshal back
		var restored SegmentEvent
		err = jsonrs.Unmarshal(data, &restored)
		require.NoError(t, err)
		require.Equal(t, segmentEvents[0].Type, restored.Type)
		require.Equal(t, segmentEvents[0].Event, restored.Event)
		require.Equal(t, segmentEvents[0].UserID, restored.UserID)
		require.Equal(t, segmentEvents[0].MessageID, restored.MessageID)
	})

	t.Run("Event type is case-insensitive", func(t *testing.T) {
		mapper := NewBaseSchemaMapper("test")

		tests := []struct {
			inputType    string
			expectedType string
		}{
			{"TRACK", "track"},
			{"Track", "track"},
			{"IDENTIFY", "identify"},
			{"Identify", "identify"},
			{"GROUP", "group"},
			{"Group", "group"},
		}

		for _, tc := range tests {
			t.Run(tc.inputType, func(t *testing.T) {
				event := Event{
					ID:        "evt-case-" + tc.inputType,
					Type:      tc.inputType,
					Name:      "test.event",
					Timestamp: time.Now(),
					Data:      map[string]interface{}{},
				}

				segmentEvents, err := mapper.MapToSegmentSpec(event)
				require.NoError(t, err)
				require.Len(t, segmentEvents, 1)
				require.Equal(t, tc.expectedType, segmentEvents[0].Type)
			})
		}
	})
}
