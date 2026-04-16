package gateway

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rudderlabs/rudder-go-kit/config"
	kithttputil "github.com/rudderlabs/rudder-go-kit/httputil"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	gwstats "github.com/rudderlabs/rudder-server/gateway/internal/stats"
	"github.com/rudderlabs/rudder-server/gateway/response"
	gwtypes "github.com/rudderlabs/rudder-server/gateway/types"
)

// sensitiveHeaders lists HTTP header names (canonical form) that must be
// stripped from the request context forwarded to the Functions runtime and
// persisted in the jobs database. This prevents credentials, session tokens,
// and authentication headers from leaking into event storage.
var sensitiveHeaders = map[string]struct{}{
	"Authorization":       {},
	"Cookie":              {},
	"Set-Cookie":          {},
	"X-Functions-Token":   {},
	"Proxy-Authorization": {},
}

// defaultMaxBodySize is the maximum request body size (in bytes) accepted by
// the Source Functions webhook handler. Requests exceeding this limit are
// sourceFunctionsWebhookHandler returns an http.HandlerFunc that processes
// Source Functions webhook requests at /v1/functions/source. It authenticates
// the request via the writeKey-based auth middleware, reads the raw webhook
// body, wraps it into a RudderStack batch event envelope, and feeds the
// resulting payload through the standard Gateway pipeline (via rrh.ProcessRequest).
//
// The handler follows the established Gateway handler factory pattern used by
// webRequestHandler, beaconBatchHandler, and webhookHandler, including:
//   - Distributed tracing via gw.tracer.Start
//   - Deferred error handling with span status and structured logging
//   - Metric tracking via TrackRequestMetrics and SourceStat
func (gw *Handle) sourceFunctionsWebhookHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		reqType := ctx.Value(gwtypes.CtxParamCallType).(string)
		arctx := ctx.Value(gwtypes.CtxParamAuthRequestContext).(*gwtypes.AuthRequestContext)

		ctx, span := gw.tracer.Start(ctx, "gw.sourceFunctionsWebhookHandler", stats.SpanKindServer,
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

				// Track failure metrics using SourceStat
				stat := gwstats.SourceStat{
					Source:        arctx.SourceTag(),
					SourceID:      arctx.SourceID,
					WriteKey:      arctx.WriteKey,
					ReqType:       reqType,
					WorkspaceID:   arctx.WorkspaceID,
					SourceType:    arctx.SourceCategory,
					SourceDefName: arctx.SourceDefName,
				}
				stat.RequestFailed(response.GetStatus(errorMessage))
				stat.Report(gw.stats)
				return
			}
		}()

		// Enforce a configurable request body size limit to prevent DoS via
		// oversized payloads (CWE-400). The limit defaults to 5 MB and is
		// configurable via Gateway.sourceFunctions.maxBodySize.
		const defaultMaxBodySize int64 = 5 * 1024 * 1024 // 5 MB
		maxBody := config.GetInt64("Gateway.sourceFunctions.maxBodySize", defaultMaxBodySize)
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)

		// Read the incoming webhook payload
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil || len(body) == 0 {
			errorMessage = response.RequestBodyNil
			return
		}

		// Build a Source Functions request envelope containing the raw webhook data.
		// The Functions runtime (functions/runtime/source_functions.go) will process
		// this via onRequest(request, settings) and return RudderStack events.
		//
		// For the initial integration, we wrap the webhook payload in a batch event
		// format that the standard pipeline can process. The Functions runtime will
		// later be invoked by the Processor via the Transformer service.
		payload, buildErr := buildSourceFunctionPayload(body, r, arctx)
		if buildErr != nil {
			errorMessage = buildErr.Error()
			return
		}

		// Process through the standard Gateway pipeline
		errorMessage = gw.rrh.ProcessRequest(&w, r, reqType, payload, arctx)
		gw.TrackRequestMetrics(errorMessage)
		if errorMessage != "" {
			return
		}

		// Track success metrics using SourceStat
		stat := gwstats.SourceStat{
			Source:        arctx.SourceTag(),
			SourceID:      arctx.SourceID,
			WriteKey:      arctx.WriteKey,
			ReqType:       reqType,
			WorkspaceID:   arctx.WorkspaceID,
			SourceType:    arctx.SourceCategory,
			SourceDefName: arctx.SourceDefName,
		}
		stat.RequestEventsSucceeded(1)
		stat.Report(gw.stats)

		responseBody := response.GetStatus(response.Ok)
		gw.logger.Debugn("response",
			logger.NewStringField("ip", kithttputil.GetRequestIP(r)),
			logger.NewStringField("path", r.URL.Path),
			logger.NewIntField("status", int64(http.StatusOK)),
			logger.NewStringField("body", responseBody))
		_, _ = w.Write([]byte(responseBody))
	}
}

// sourceFunctionEvent represents a single event in the batch payload envelope
// generated for Source Functions webhook processing. It mirrors the standard
// RudderStack event schema with additional context.request metadata so that the
// Functions runtime can access the original HTTP request details.
type sourceFunctionEvent struct {
	Type        string                 `json:"type"`
	Event       string                 `json:"event"`
	Properties  stdjson.RawMessage     `json:"properties"`
	Context     map[string]interface{} `json:"context"`
	AnonymousID string                 `json:"anonymousId"`
	MessageID   string                 `json:"messageId"`
	Timestamp   string                 `json:"timestamp"`
}

// sourceFunctionBatch is the top-level batch envelope sent to the Gateway pipeline.
type sourceFunctionBatch struct {
	Batch []sourceFunctionEvent `json:"batch"`
}

// buildSourceFunctionPayload constructs a RudderStack batch event payload from
// the raw webhook HTTP request body. The payload wraps the original body as
// event properties and enriches the context with the full HTTP request metadata
// (method, URL, headers, raw body) so that the Functions runtime can invoke
// the Source Function's onRequest(request, settings) handler with complete
// request information.
//
// The generated payload follows this structure:
//
//	{
//	  "batch": [{
//	    "type": "track",
//	    "event": "Source Function Webhook",
//	    "properties": { <raw webhook JSON or {"body": "<raw string>"} for non-JSON> },
//	    "context": {
//	      "source": "sourceFunctions",
//	      "library": {"name": "rudder-server/source-functions", "version": "1.0.0"},
//	      "request": {
//	        "method": "<HTTP method>",
//	        "url": "<request URL path>",
//	        "headers": { <request headers as map> },
//	        "body": "<raw body string>"
//	      },
//	      "sourceSettings": <source config from backend-config>
//	    },
//	    "anonymousId": "<generated UUID>",
//	    "messageId": "<generated UUID>",
//	    "timestamp": "<ISO 8601 timestamp>"
//	  }]
//	}
func buildSourceFunctionPayload(body []byte, r *http.Request, arctx *gwtypes.AuthRequestContext) ([]byte, error) {
	// Determine properties: use raw JSON if the body is valid JSON, otherwise
	// wrap the raw body string as a "body" field.
	var properties stdjson.RawMessage
	if jsonrs.Default.Valid(body) {
		properties = body
	} else {
		wrapped := map[string]string{"body": string(body)}
		var marshalErr error
		properties, marshalErr = jsonrs.Marshal(wrapped)
		if marshalErr != nil {
			return nil, fmt.Errorf("%s: %w", response.ErrorInMarshal, marshalErr)
		}
	}

	// Build the headers map from the request, stripping sensitive headers
	// (Authorization, Cookie, X-Functions-Token, etc.) to prevent credential
	// leakage into the event context and jobs database (CWE-200).
	headers := make(map[string]string, len(r.Header))
	for key, values := range r.Header {
		if len(values) > 0 {
			canonicalKey := http.CanonicalHeaderKey(key)
			if _, isSensitive := sensitiveHeaders[canonicalKey]; isSensitive {
				continue
			}
			// Also skip headers with common sensitive prefixes
			lower := strings.ToLower(key)
			if strings.HasPrefix(lower, "x-api-key") || strings.HasPrefix(lower, "x-auth") {
				continue
			}
			headers[key] = values[0]
		}
	}

	// Build the context including request metadata and source settings
	requestContext := map[string]interface{}{
		"method":  r.Method,
		"url":     r.URL.Path,
		"headers": headers,
		"body":    string(body),
	}

	eventContext := map[string]interface{}{
		"source": "sourceFunctions",
		"library": map[string]string{
			"name":    "rudder-server/source-functions",
			"version": "1.0.0",
		},
		"request": requestContext,
	}

	// Include source settings from backend-config so the Functions runtime
	// has access to the source's configuration (e.g., function code, settings)
	if len(arctx.SourceDetails.Config) > 0 {
		eventContext["sourceSettings"] = arctx.SourceDetails.Config
	}

	now := time.Now().UTC()
	event := sourceFunctionEvent{
		Type:        "track",
		Event:       "Source Function Webhook",
		Properties:  properties,
		Context:     eventContext,
		AnonymousID: uuid.New().String(),
		MessageID:   uuid.New().String(),
		Timestamp:   now.Format(time.RFC3339Nano),
	}

	batch := sourceFunctionBatch{
		Batch: []sourceFunctionEvent{event},
	}

	payload, err := jsonrs.Marshal(batch)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", response.ErrorInMarshal, err)
	}
	return payload, nil
}
