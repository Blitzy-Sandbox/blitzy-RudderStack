package cloudsources

import (
	"time"
)

// Ingestion mode constants define how a cloud source connector receives data
// from the third-party service.
const (
	// ModePolling is the polling ingestion mode constant.
	// Polling-based sources periodically fetch data from REST APIs.
	ModePolling = "polling"

	// ModeWebhook is the webhook ingestion mode constant.
	// Webhook-based sources receive real-time event callbacks via HTTP.
	ModeWebhook = "webhook"
)

// Default configuration constants provide sensible production defaults
// for cloud source polling connectors. These defaults are referenced by
// NewDefaultPollingConfig and can be overridden per-source via CloudSourceConfig.
const (
	// DefaultPollingInterval is the default duration between polling cycles.
	// Set to 5 minutes to balance freshness against API rate limits for
	// typical cloud source APIs (e.g., Salesforce, HubSpot).
	DefaultPollingInterval = 5 * time.Minute

	// DefaultMaxRetries is the default maximum number of retry attempts
	// per polling cycle before marking the cycle as failed.
	DefaultMaxRetries = 3

	// DefaultRateLimit is the default rate limit expressed as the maximum
	// number of API requests per minute. Set conservatively to stay within
	// most third-party API rate limit tiers.
	DefaultRateLimit = 60

	// DefaultPageSize is the default number of records to fetch per API call.
	// Aligns with common SaaS API pagination defaults (e.g., Stripe, HubSpot).
	DefaultPageSize = 100

	// DefaultTimeout is the default maximum duration for a single API call.
	// Set to 30 seconds to accommodate slow responses without blocking
	// the polling loop indefinitely.
	DefaultTimeout = 30 * time.Second
)

// CloudSourceConfig is the top-level configuration for a cloud source connector.
// It defines the source identity, connector type, ingestion mode (polling or webhook),
// and nested configuration for credentials, polling, and webhook settings.
//
// This structure mirrors the backend-config SourceT pattern (backend-config/types.go)
// with cloud-source-specific extensions for credential management, polling cadence,
// and webhook HMAC validation. It is designed for encrypted credential storage and
// runtime-only secret injection — no credentials are hardcoded.
type CloudSourceConfig struct {
	// ID is the unique identifier for this cloud source instance.
	ID string `json:"id"`

	// Name is the human-readable display name for this source.
	Name string `json:"name"`

	// SourceType identifies the connector type (e.g., "stripe", "salesforce", "hubspot").
	// This value is used for registry lookup and schema mapping resolution.
	SourceType string `json:"sourceType"`

	// Mode is the ingestion mode: ModePolling ("polling") or ModeWebhook ("webhook").
	// Determines which nested configuration block (Polling or Webhook) is active.
	Mode string `json:"mode"`

	// WorkspaceID is the RudderStack workspace this source belongs to.
	// Used for multi-tenant isolation and event attribution.
	WorkspaceID string `json:"workspaceId"`

	// WriteKey is the source write key for event attribution in the
	// RudderStack pipeline. Events ingested by this source are tagged
	// with this write key for routing and access control.
	WriteKey string `json:"writeKey"`

	// Enabled controls whether this source is active. Disabled sources
	// stop polling and reject incoming webhooks.
	Enabled bool `json:"enabled"`

	// Credentials holds encrypted credential configuration for authenticating
	// with the third-party cloud source API. Credentials are never stored in
	// plaintext — they are injected at runtime from encrypted storage.
	Credentials CredentialConfig `json:"credentials"`

	// Polling holds polling-specific configuration. This block is used when
	// Mode is set to ModePolling. It controls polling interval, pagination,
	// rate limiting, and retry behavior.
	Polling PollingConfig `json:"polling,omitempty"`

	// Webhook holds webhook-specific configuration. This block is used when
	// Mode is set to ModeWebhook. It controls the webhook endpoint URL,
	// HMAC signature validation, and signature header name.
	Webhook WebhookConfig `json:"webhook,omitempty"`
}

// CredentialConfig defines encrypted credential storage for cloud source connectors.
// Credentials are never hardcoded — they are injected at runtime from encrypted
// storage. This follows the security requirement for cloud source credential
// management: encrypted storage, runtime-only secret injection, and credential
// rotation support.
//
// The struct supports both API key-based authentication (APIKey/APISecret) and
// OAuth 2.0 token-based authentication (AccessToken/RefreshToken). Connectors
// use whichever credential fields their source API requires.
type CredentialConfig struct {
	// APIKey is the encrypted API key for the cloud source.
	// Used by sources that authenticate via API key (e.g., Stripe, SendGrid).
	APIKey string `json:"apiKey,omitempty"`

	// APISecret is the encrypted API secret paired with the API key.
	// Used by sources requiring key+secret authentication (e.g., Twilio).
	APISecret string `json:"apiSecret,omitempty"`

	// AccessToken is the encrypted OAuth 2.0 access token.
	// Used by sources that authenticate via OAuth (e.g., Salesforce, HubSpot).
	AccessToken string `json:"accessToken,omitempty"`

	// RefreshToken is the encrypted OAuth 2.0 refresh token.
	// Used to obtain new access tokens when they expire without requiring
	// re-authentication by the user.
	RefreshToken string `json:"refreshToken,omitempty"`

	// IsEncrypted indicates whether the credential values in this config are
	// currently encrypted. When true, credential values must be decrypted at
	// runtime before use. When false, values are in plaintext (e.g., during
	// local development only — never in production).
	IsEncrypted bool `json:"isEncrypted"`
}

// PollingConfig defines configuration for polling-based cloud source connectors.
// Polling-based sources periodically fetch data from third-party REST APIs using
// cursor-based pagination. This configuration controls the polling cadence, retry
// behavior, rate limiting, and pagination parameters.
//
// The pattern follows the established FeaturesServiceOptions approach
// (services/transformer/features.go) with PollInterval and retry semantics,
// extended with rate limiting and cursor management for cloud source APIs.
type PollingConfig struct {
	// Interval is the duration between polling cycles. Each cycle fetches new
	// data from the source API starting from the last known cursor position.
	// Defaults to DefaultPollingInterval (5 minutes) if zero.
	Interval time.Duration `json:"interval"`

	// MaxRetries is the maximum number of retry attempts per polling cycle
	// before marking the cycle as failed and waiting for the next interval.
	// Defaults to DefaultMaxRetries (3) if zero.
	MaxRetries int `json:"maxRetries"`

	// RateLimit is the maximum number of API requests per minute. The poller
	// throttles API calls to stay within this limit, preventing rate limit
	// errors from the source API.
	// Defaults to DefaultRateLimit (60) if zero.
	RateLimit int `json:"rateLimit"`

	// InitialCursor is the starting cursor for pagination on first run.
	// The format is source-specific: it may be a timestamp, offset, page
	// token, or other pagination marker. An empty string means "start from
	// the beginning" or "use the source's default start position."
	InitialCursor string `json:"initialCursor,omitempty"`

	// PageSize is the number of records to fetch per API call. Larger page
	// sizes reduce the number of API calls but increase response payload size.
	// Defaults to DefaultPageSize (100) if zero.
	PageSize int `json:"pageSize"`

	// Timeout is the maximum duration for a single API call. Requests that
	// exceed this timeout are cancelled and retried (up to MaxRetries).
	// Defaults to DefaultTimeout (30 seconds) if zero.
	Timeout time.Duration `json:"timeout"`
}

// WebhookConfig defines configuration for webhook-based cloud source connectors.
// Webhook-based sources receive real-time events via HTTP callbacks from third-party
// services (e.g., Stripe webhooks, SendGrid event hooks, GitHub webhooks).
//
// This configuration follows the security requirement for HMAC webhook signature
// validation to prevent webhook spoofing. The HMACSecret is never hardcoded —
// it is injected from encrypted storage at runtime.
type WebhookConfig struct {
	// URL is the webhook endpoint URL that the cloud source service registers
	// with the third-party provider. The provider POSTs events to this URL.
	URL string `json:"url"`

	// HMACSecret is the shared secret for HMAC signature validation.
	// This value must never be hardcoded — it is injected from encrypted
	// storage at runtime. Used to compute HMAC-SHA256 signatures for
	// verifying the authenticity of inbound webhook requests.
	HMACSecret string `json:"hmacSecret,omitempty"`

	// SignatureHeader is the HTTP header name containing the HMAC signature
	// on inbound webhook requests. This is source-specific:
	//   - Stripe: "Stripe-Signature"
	//   - SendGrid: "X-Twilio-Email-Event-Webhook-Signature"
	//   - GitHub: "X-Hub-Signature-256"
	//   - Default: "X-Webhook-Signature"
	SignatureHeader string `json:"signatureHeader"`

	// ValidateSignature controls whether HMAC signature validation is enforced
	// on inbound webhook requests. When true, requests without a valid
	// signature are rejected. Should always be true in production — set to
	// false only for local development/testing.
	ValidateSignature bool `json:"validateSignature"`
}

// NewDefaultPollingConfig returns a PollingConfig populated with all default values.
// This provides a sensible starting configuration for polling-based cloud source
// connectors. Individual fields can be overridden after creation.
//
// Default values:
//   - Interval:      5 minutes (DefaultPollingInterval)
//   - MaxRetries:    3 (DefaultMaxRetries)
//   - RateLimit:     60 requests/minute (DefaultRateLimit)
//   - PageSize:      100 records/page (DefaultPageSize)
//   - Timeout:       30 seconds (DefaultTimeout)
//   - InitialCursor: empty (start from the beginning)
func NewDefaultPollingConfig() PollingConfig {
	return PollingConfig{
		Interval:      DefaultPollingInterval,
		MaxRetries:    DefaultMaxRetries,
		RateLimit:     DefaultRateLimit,
		InitialCursor: "",
		PageSize:      DefaultPageSize,
		Timeout:       DefaultTimeout,
	}
}

// NewDefaultWebhookConfig returns a WebhookConfig with reasonable defaults
// for webhook-based cloud source connectors. Signature validation is enabled
// by default for security, using the generic "X-Webhook-Signature" header.
// Connectors should override SignatureHeader with their source-specific header
// name (e.g., "Stripe-Signature" for Stripe).
//
// Default values:
//   - ValidateSignature: true (enforced for security)
//   - SignatureHeader:   "X-Webhook-Signature" (generic default)
//   - URL:               empty (must be configured per deployment)
//   - HMACSecret:        empty (injected at runtime from encrypted storage)
func NewDefaultWebhookConfig() WebhookConfig {
	return WebhookConfig{
		ValidateSignature: true,
		SignatureHeader:   "X-Webhook-Signature",
	}
}
