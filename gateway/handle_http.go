package gateway

import (
	"context"
	"net/http"
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

// requireBearerAuth is a middleware that enforces the presence of a valid Authorization
// header with a Bearer token format on management API endpoints. This provides gateway-level
// authentication as a defense-in-depth layer to ensure management endpoints (Protocols,
// Profiles, Monitoring) are not publicly accessible without credentials.
//
// The middleware validates the header format only (Bearer <token> with non-empty token).
// Actual token validation and authorization is delegated to the internal handler, which
// has access to workspace context and backend-config for token verification.
//
// Returns HTTP 401 Unauthorized with WWW-Authenticate challenge if the Authorization
// header is missing, malformed, or contains an empty token.
// requireBearerAuth wraps handler with Bearer token authentication check.
// Used by webProtocolsHandler, webProfilesHandler, webMonitoringHandler —
// these handler factory methods are defined but awaiting route mounting in
// handle_lifecycle.go (future checkpoint).
func requireBearerAuth(delegate http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="RudderStack API"`)
			http.Error(w, "Authorization header required", http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(authHeader, "Bearer ") || strings.TrimSpace(authHeader[7:]) == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="RudderStack API"`)
			http.Error(w, "Invalid or missing Bearer token", http.StatusUnauthorized)
			return
		}
		delegate.ServeHTTP(w, r)
	}
}

// withContentType sets the content type of the response to the given value
func withContentType(contentType string, delegate http.HandlerFunc) http.HandlerFunc { // nolint: unparam
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", contentType)
		delegate(w, r)
	}
}
