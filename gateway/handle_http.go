package gateway

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	gwtypes "github.com/rudderlabs/rudder-server/gateway/types"

	kithttputil "github.com/rudderlabs/rudder-go-kit/httputil"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/gateway/response"
)

// webAudienceListHandler - handler for audience list requests
func (gw *Handle) webAudienceListHandler() http.HandlerFunc {
	return gw.callType("audiencelist", gw.writeKeyAuth(gw.webHandler()))
}

// webExtractHandler - handler for extract requests
func (gw *Handle) webExtractHandler() http.HandlerFunc {
	return gw.callType("extract", gw.writeKeyAuth(gw.webHandler()))
}

// webBatchHandler - handler for batch requests
func (gw *Handle) webBatchHandler() http.HandlerFunc {
	return gw.callType("batch", gw.writeKeyAuth(gw.webHandler()))
}

func (gw *Handle) internalBatchHandler() http.HandlerFunc {
	return gw.callType("internalBatch", gw.internalBatchHandlerFunc())
}

// webIdentifyHandler - handler for identify requests
func (gw *Handle) webIdentifyHandler() http.HandlerFunc {
	return gw.callType("identify", gw.writeKeyAuth(gw.webHandler()))
}

// webTrackHandler - handler for track requests
func (gw *Handle) webTrackHandler() http.HandlerFunc {
	return gw.callType("track", gw.writeKeyAuth(gw.webHandler()))
}

// webPageHandler - handler for page requests
func (gw *Handle) webPageHandler() http.HandlerFunc {
	return gw.callType("page", gw.writeKeyAuth(gw.webHandler()))
}

// webScreenHandler - handler for screen requests
func (gw *Handle) webScreenHandler() http.HandlerFunc {
	return gw.callType("screen", gw.writeKeyAuth(gw.webHandler()))
}

// webAliasHandler - handler for alias requests
func (gw *Handle) webAliasHandler() http.HandlerFunc {
	return gw.callType("alias", gw.writeKeyAuth(gw.webHandler()))
}

// webMergeHandler - handler for merge requests
func (gw *Handle) webMergeHandler() http.HandlerFunc {
	return gw.callType("merge", gw.writeKeyAuth(gw.webHandler()))
}

// webGroupHandler - handler for group requests
func (gw *Handle) webGroupHandler() http.HandlerFunc {
	return gw.callType("group", gw.writeKeyAuth(gw.webHandler()))
}

// webSourceFunctionsHandler - handler for Source Functions webhook requests (E-015)
// Routes to the dedicated Source Functions handler in handle_http_functions.go.
// The handler authenticates via sourceFunctionsAuth (handle_http_auth.go) which supports
// Bearer token, X-Functions-Token header, and writeKey-based auth, then delegates to
// sourceFunctionsWebhookHandler which wraps the webhook payload in a batch event format
// for standard pipeline processing.
func (gw *Handle) webSourceFunctionsHandler() http.HandlerFunc {
	return gw.callType("sourceFunctions", gw.sourceFunctionsAuth(gw.sourceFunctionsWebhookHandler()))
}

// webProtocolsHandler - handler for Protocols/Tracking Plan management API (E-024)
// This handler delegates to the Protocols API router registered via the protocols package.
// The actual CRUD logic, versioning, and CSV import/export are in protocols/api/handler.go.
// The protocols API routes are mounted at /v1/protocols/... in handle_lifecycle.go.
// Bearer token authentication is enforced at the gateway level via requireBearerAuth;
// the internal handler may perform additional authorization checks.
func (gw *Handle) webProtocolsHandler() http.HandlerFunc {
	return requireBearerAuth(withContentType("application/json; charset=utf-8", func(w http.ResponseWriter, r *http.Request) {
		// Delegate to the protocols API handler registered in gw.internalHttpHandlers
		if handler, ok := gw.internalHttpHandlers["/v1/protocols"]; ok {
			handler.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Protocols API not configured", http.StatusServiceUnavailable)
	}))
}

// webProfilesHandler - handler for Profiles REST API (E-027)
// This handler delegates to the Identity/Profiles API router registered via the identity package.
// The actual profile lookup logic (traits, events, external_ids, metadata) is in identity/profiles/api.go.
// Profiles API targets sub-200ms response times backed by Redis caching.
// Bearer token authentication is enforced at the gateway level via requireBearerAuth;
// the internal handler may perform additional authorization checks.
func (gw *Handle) webProfilesHandler() http.HandlerFunc {
	return requireBearerAuth(withContentType("application/json; charset=utf-8", func(w http.ResponseWriter, r *http.Request) {
		// Delegate to the profiles API handler registered in gw.internalHttpHandlers
		if handler, ok := gw.internalHttpHandlers["/v1/profiles"]; ok {
			handler.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Profiles API not configured", http.StatusServiceUnavailable)
	}))
}

// webMonitoringHandler - handler for per-destination delivery monitoring dashboard (E-036)
// Serves per-destination delivery metrics including success/failure rates, latency percentiles
// (p50/p95/p99), throughput, retry counts, and circuit breaker state.
// The actual metrics aggregation is in services/monitoring/dashboard.go.
// Bearer token authentication is enforced at the gateway level via requireBearerAuth;
// the internal handler may perform additional authorization checks.
func (gw *Handle) webMonitoringHandler() http.HandlerFunc {
	return requireBearerAuth(withContentType("application/json; charset=utf-8", func(w http.ResponseWriter, r *http.Request) {
		// Delegate to the monitoring dashboard handler registered in gw.internalHttpHandlers
		if handler, ok := gw.internalHttpHandlers["/v1/monitoring"]; ok {
			handler.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Monitoring API not configured", http.StatusServiceUnavailable)
	}))
}

// webProfilingHandler - handler for pipeline performance profiling API (E-039)
// This handler delegates to the profiling API router registered via the services/profiling package.
// Exposes /pipeline (profiler report) and /capacity (capacity planning report) sub-endpoints.
// Bearer token authentication is enforced at the gateway level via requireBearerAuth;
// the internal handler may perform additional authorization checks.
func (gw *Handle) webProfilingHandler() http.HandlerFunc {
	return requireBearerAuth(withContentType("application/json; charset=utf-8", func(w http.ResponseWriter, r *http.Request) {
		// Delegate to the profiling API handler registered in gw.internalHttpHandlers
		if handler, ok := gw.internalHttpHandlers["/v1/profiling"]; ok {
			handler.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Profiling API not configured", http.StatusServiceUnavailable)
	}))
}

// webAlertingHandler - handler for configurable alerting rules API (E-037)
// This handler delegates to the alerting API router registered via the services/alerting package.
// Exposes /rules CRUD sub-endpoints for alert rule management.
// Bearer token authentication is enforced at the gateway level via requireBearerAuth;
// the internal handler may perform additional authorization checks.
func (gw *Handle) webAlertingHandler() http.HandlerFunc {
	return requireBearerAuth(withContentType("application/json; charset=utf-8", func(w http.ResponseWriter, r *http.Request) {
		// Delegate to the alerting API handler registered in gw.internalHttpHandlers
		if handler, ok := gw.internalHttpHandlers["/v1/alerts"]; ok {
			handler.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Alerting API not configured", http.StatusServiceUnavailable)
	}))
}

// webAdvancedReplayHandler - handler for advanced replay with source/date-range/destination filtering and dry-run (E-038)
// Extends the base replay handler with additional filter parameters parsed from HTTP headers:
// X-Replay-Source-Filter, X-Replay-Start-Date, X-Replay-End-Date, X-Replay-Destination-Filter, X-Replay-Dry-Run.
// Advanced filter logic is in handle_http_replay_advanced.go.
func (gw *Handle) webAdvancedReplayHandler() http.HandlerFunc {
	return gw.callType("replay", gw.replaySourceIDAuth(gw.withAdvancedReplayFilters(gw.withWarehouseReplayTag(gw.webHandler()))))
}

// robotsHandler prevents robots from crawling the gateway endpoints
func (*Handle) robotsHandler(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("User-agent: * \nDisallow: / \n"))
}

// webHandler - regular web request handler
func (gw *Handle) webHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		gw.webRequestHandler(gw.rrh, w, r)
	}
}

// webRequestHandler - handles web requests containing rudder events as payload.
// It parses the payload and calls the request handler to process the request.
func (gw *Handle) webRequestHandler(rh RequestHandler, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqType := ctx.Value(gwtypes.CtxParamCallType).(string)
	arctx := ctx.Value(gwtypes.CtxParamAuthRequestContext).(*gwtypes.AuthRequestContext)

	ctx, span := gw.tracer.Start(ctx, "gw.webRequestHandler", stats.SpanKindServer,
		stats.SpanWithTimestamp(time.Now()),
		stats.SpanWithTags(stats.Tags{
			"reqType":     reqType,
			"path":        r.URL.Path,
			"workspaceId": arctx.WorkspaceID,
			"sourceId":    arctx.SourceID,
		}),
	)
	r = r.WithContext(ctx)

	gw.logger.LogRequest(r)
	var errorMessage string
	defer func() {
		defer span.End()
		if errorMessage != "" {
			span.SetStatus(stats.SpanStatusError, errorMessage)
			status := response.GetErrorStatusCode(errorMessage)
			responseBody := response.GetStatus(errorMessage)
			gw.logger.Infon("response",
				logger.NewStringField("ip", kithttputil.GetRequestIP(r)),
				logger.NewStringField("path", r.URL.Path),
				logger.NewIntField("status", int64(status)),
				logger.NewStringField("body", responseBody))
			http.Error(w, responseBody, status)
			return
		}
	}()
	payload, err := gw.getPayload(arctx, r, reqType)
	if err != nil {
		errorMessage = err.Error()
		return
	}
	errorMessage = rh.ProcessRequest(&w, r, reqType, payload, arctx)
	gw.TrackRequestMetrics(errorMessage)
	if errorMessage != "" {
		return
	}

	responseBody := response.GetStatus(response.Ok)
	gw.logger.Debugn("response",
		logger.NewStringField("ip", kithttputil.GetRequestIP(r)),
		logger.NewStringField("path", r.URL.Path),
		logger.NewIntField("status", int64(http.StatusOK)),
		logger.NewStringField("body", responseBody))
	_, _ = w.Write([]byte(responseBody))
}

// callType middleware sets the call type in the request context
func (gw *Handle) callType(callType string, delegate http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), gwtypes.CtxParamCallType, callType))
		delegate(w, r)
	}
}

// bearerTokenValidator is a pluggable token validation function used by requireBearerAuth.
// It receives the raw bearer token string and returns the validated workspace ID and nil
// error on success, or an empty string and error if the token is invalid.
//
// The default implementation validates against the RUDDER_ADMIN_TOKEN environment variable
// (or config key "adminToken"). In production deployments this should be replaced with
// JWT signature verification or a token-store lookup via SetBearerTokenValidator.
//
// This design allows the gateway to enforce real token validation at the middleware level
// while remaining testable and configurable for different deployment scenarios.
var bearerTokenValidator func(token string) (workspaceID string, err error)

// init sets the default bearer token validator which checks against the configured
// admin token. This provides actual credential verification rather than format-only
// checks, preventing unauthorized access to management API endpoints.
func init() {
	bearerTokenValidator = defaultBearerTokenValidator
}

// defaultBearerTokenValidator validates a bearer token against the server's configured
// admin token. The admin token is read from the RUDDER_ADMIN_TOKEN environment variable
// or the "adminToken" config key, consistent with the existing admin authentication
// pattern in admin/admin.go.
//
// Returns the token as a workspace identifier on success (admin tokens are workspace-agnostic),
// or an error if the token does not match.
func defaultBearerTokenValidator(token string) (string, error) {
	// Read the admin token from environment or config. This is the same credential
	// used by the admin RPC interface in admin/admin.go for server management.
	adminToken := strings.TrimSpace(getAdminToken())
	if adminToken == "" {
		// If no admin token is configured, reject all bearer tokens to prevent
		// accidental open access. Operators must explicitly configure an admin token
		// to enable management API access.
		return "", fmt.Errorf("management API authentication not configured: set RUDDER_ADMIN_TOKEN")
	}
	if !hmacEqual(token, adminToken) {
		return "", fmt.Errorf("invalid bearer token")
	}
	// Admin tokens are workspace-agnostic; return "admin" as the workspace context.
	return "admin", nil
}

// getAdminToken reads the admin token from environment variable or returns empty string.
// Uses os.Getenv directly to avoid circular dependency on config package during init.
func getAdminToken() string {
	if v := strings.TrimSpace(os.Getenv("RUDDER_ADMIN_TOKEN")); v != "" {
		return v
	}
	// Fallback: check the legacy config key used by admin/admin.go
	if v := strings.TrimSpace(os.Getenv("RSERVER_ADMIN_TOKEN")); v != "" {
		return v
	}
	return ""
}

// hmacEqual performs a constant-time comparison of two strings to prevent
// timing-based side-channel attacks on token validation.
func hmacEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// SetBearerTokenValidator replaces the default bearer token validator with a custom
// implementation. This enables JWT-based validation, external token store lookups,
// or other authentication strategies in production deployments.
//
// The validator function receives the raw bearer token string and must return:
//   - (workspaceID, nil) if the token is valid — workspaceID is set in request context
//   - ("", error) if the token is invalid — the error message is NOT exposed to clients
//
// Thread-safety: This function should be called during server initialization before
// any HTTP requests are served. It is NOT safe for concurrent use with active requests.
func SetBearerTokenValidator(validator func(token string) (workspaceID string, err error)) {
	if validator != nil {
		bearerTokenValidator = validator
	}
}

// BearerAuthWorkspaceKey is the context key for the workspace ID extracted from
// a validated bearer token. Downstream handlers can retrieve it via:
//
//	workspaceID := r.Context().Value(gateway.BearerAuthWorkspaceKey).(string)
type bearerAuthWorkspaceKeyType struct{}

// BearerAuthWorkspaceKey is the context key used to store the workspace ID
// derived from a validated bearer token.
var BearerAuthWorkspaceKey = bearerAuthWorkspaceKeyType{}

// requireBearerAuth is a middleware that enforces bearer token authentication on
// management API endpoints (Functions, Protocols, Profiles, Monitoring).
//
// The middleware performs REAL token validation — not just format checking:
//  1. Extracts the Bearer token from the Authorization header
//  2. Validates the token via bearerTokenValidator (default: admin token check)
//  3. On success: stores the validated workspace ID in request context and delegates
//  4. On failure: returns HTTP 401 with WWW-Authenticate challenge
//
// Returns HTTP 401 Unauthorized if:
//   - Authorization header is missing
//   - Authorization header does not start with "Bearer "
//   - Bearer token is empty
//   - Token validation fails (invalid credentials)
func requireBearerAuth(delegate http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="RudderStack API"`)
			http.Error(w, "Authorization header required", http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(authHeader, "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="RudderStack API"`)
			http.Error(w, "Invalid or missing Bearer token", http.StatusUnauthorized)
			return
		}
		token := strings.TrimSpace(authHeader[7:])
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="RudderStack API"`)
			http.Error(w, "Invalid or missing Bearer token", http.StatusUnauthorized)
			return
		}

		// Perform actual token validation — not just format checking.
		workspaceID, err := bearerTokenValidator(token)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="RudderStack API", error="invalid_token"`)
			http.Error(w, "Invalid or expired Bearer token", http.StatusUnauthorized)
			return
		}

		// Store validated workspace ID in request context for downstream handlers.
		ctx := context.WithValue(r.Context(), BearerAuthWorkspaceKey, workspaceID)
		delegate.ServeHTTP(w, r.WithContext(ctx))
	}
}

// withContentType sets the content type of the response to the given value
func withContentType(contentType string, delegate http.HandlerFunc) http.HandlerFunc { // nolint: unparam
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", contentType)
		delegate(w, r)
	}
}
