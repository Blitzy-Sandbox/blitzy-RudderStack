package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// webReplayHandler can handle replay requests.
// For E-035 (Warehouse Replay), this handler detects the X-Warehouse-Replay header
// and tags events with a warehouseOnly routing flag in their context before passing
// to the standard web handler pipeline.
func (gw *Handle) webReplayHandler() http.HandlerFunc {
	return gw.callType("replay", gw.replaySourceIDAuth(gw.withWarehouseReplayTag(gw.webHandler())))
}

// webReplayAdvancedHandler handles advanced replay requests with source-level,
// date-range, destination-level filtering and dry-run mode (E-038).
// The handler chain adds withAdvancedReplayFilters (defined in handle_http_replay_advanced.go)
// before the existing withWarehouseReplayTag middleware, enabling advanced filter parameters
// to be injected into event context via HTTP headers:
//
//   - X-Replay-Source-Filter: Source ID to filter replay events
//   - X-Replay-Start-Date / X-Replay-End-Date: Date range for replay window (RFC 3339)
//   - X-Replay-Destination-Filter: Destination ID to target replay
//   - X-Replay-Dry-Run: Preview mode without executing side effects
//
// This handler is intended for the /v1/replay/advanced endpoint.
func (gw *Handle) webReplayAdvancedHandler() http.HandlerFunc {
	return gw.callType("replay", gw.replaySourceIDAuth(gw.withAdvancedReplayFilters(gw.withWarehouseReplayTag(gw.webHandler()))))
}

// withWarehouseReplayTag is a middleware that detects the X-Warehouse-Replay HTTP header
// and injects a warehouseOnly routing flag into each event's context within the request body.
// This enables the Processor (processor/processor.go) to detect warehouse-targeted replay
// events and route them exclusively to warehouse destinations, bypassing real-time Router delivery.
//
// When the header is absent, the request passes through unchanged — maintaining full
// backward compatibility with existing replay requests.
//
// Request body JSON mutation:
//   - For batch format (replay requests): injects "warehouseOnly": true into each event's "context" object
//   - For single event format: injects "warehouseOnly": true into the top-level "context" object
//
// The warehouseOnly flag is preserved through the pipeline because the full event map
// (including context) flows into the job payload stored in JobsDB and processed by the Processor.
func (gw *Handle) withWarehouseReplayTag(delegate http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only intercept when the warehouse replay header is present and set to "true".
		// Use case-insensitive comparison per RFC 7230: HTTP header values for boolean
		// flags should be handled case-insensitively to accept "true", "True", "TRUE", etc.
		if !strings.EqualFold(r.Header.Get("X-Warehouse-Replay"), "true") {
			delegate.ServeHTTP(w, r)
			return
		}

		// Read the request body to inject the warehouse replay flag
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil || len(body) == 0 {
			// On read error or empty body, pass through without modification.
			// The downstream handler (webHandler → getPayloadFromRequest) will
			// handle the error appropriately.
			if len(body) > 0 {
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			delegate.ServeHTTP(w, r)
			return
		}

		// Inject warehouseOnly: true into each event's context.
		// Replay requests use batch format: {"batch": [{...}, {...}]}
		// The "batch" array is the standard format for replay callType
		// (see handle.go: reqType != "batch" && reqType != "replay" && reqType != "retl").
		batch := gjson.GetBytes(body, "batch")
		if batch.Exists() && batch.IsArray() {
			for i := range batch.Array() {
				path := "batch." + strconv.Itoa(i) + ".context.warehouseOnly"
				if modified, setErr := sjson.SetBytes(body, path, true); setErr == nil {
					body = modified
				}
			}
		} else {
			// Single event format (non-batch replay — defensive handling)
			if modified, setErr := sjson.SetBytes(body, "context.warehouseOnly", true); setErr == nil {
				body = modified
			}
		}

		// Note: Advanced replay filters (source-level, date-range, destination-level, dry-run)
		// are handled exclusively by withAdvancedReplayFilters (handle_http_replay_advanced.go).
		// For the advanced handler chain (withAdvancedReplayFilters → withWarehouseReplayTag),
		// filter injection occurs once in withAdvancedReplayFilters before this middleware runs,
		// avoiding duplicate injection. For the basic replay chain (withWarehouseReplayTag only),
		// advanced filter headers are not processed — users should use the /v1/replay/advanced
		// endpoint for filter support.

		// Replace the request body with the modified payload
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		delegate.ServeHTTP(w, r)
	}
}
