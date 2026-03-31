package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rudderlabs/rudder-go-kit/logger"
)

// isDryRunReplay checks whether the incoming HTTP request is marked as a dry-run replay.
// It reads the X-Replay-Dry-Run header and returns true when the header value is "true"
// (case-insensitive comparison using strings.EqualFold, consistent with the boolean header
// pattern used by withWarehouseReplayTag for X-Warehouse-Replay).
//
// This helper is intended for use by the Processor to detect dry-run mode on replay events
// so that actual side effects (destination delivery, archival writes) can be skipped while
// still returning a preview of what would be processed.
func isDryRunReplay(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("X-Replay-Dry-Run"), "true")
}

// injectReplayFilters injects advanced replay filter values into a JSON event body at the
// given context path prefix (e.g. "context." for single events or "batch.0.context." for
// batched events). Each non-empty filter value is set using sjson.SetBytes; errors from
// sjson.SetBytes are silently ignored per the defensive pattern established by
// withWarehouseReplayTag in handle_http_replay.go.
//
// Filter mapping:
//
//	sourceFilter  → <prefix>replaySourceFilter          (string)
//	startDate     → <prefix>replayDateRange.startDate   (string)
//	endDate       → <prefix>replayDateRange.endDate     (string)
//	destFilter    → <prefix>replayDestinationFilter     (string)
//	dryRunStr     → <prefix>replayDryRun                (bool true, only when dryRunStr == "true")
func injectReplayFilters(body []byte, prefix, sourceFilter, startDate, endDate, destFilter, dryRunStr string) []byte {
	if sourceFilter != "" {
		if modified, err := sjson.SetBytes(body, prefix+"replaySourceFilter", sourceFilter); err == nil {
			body = modified
		}
	}
	if startDate != "" {
		if modified, err := sjson.SetBytes(body, prefix+"replayDateRange.startDate", startDate); err == nil {
			body = modified
		}
	}
	if endDate != "" {
		if modified, err := sjson.SetBytes(body, prefix+"replayDateRange.endDate", endDate); err == nil {
			body = modified
		}
	}
	if destFilter != "" {
		if modified, err := sjson.SetBytes(body, prefix+"replayDestinationFilter", destFilter); err == nil {
			body = modified
		}
	}
	if strings.EqualFold(dryRunStr, "true") {
		if modified, err := sjson.SetBytes(body, prefix+"replayDryRun", true); err == nil {
			body = modified
		}
	}
	return body
}

// withAdvancedReplayFilters is an HTTP middleware that extends the base replay handler with
// source-level, date-range, destination-level filtering and dry-run mode support (E-038).
//
// It reads five optional HTTP headers and injects corresponding fields into each event's
// context object within the request body JSON:
//
//	X-Replay-Source-Filter      → context.replaySourceFilter       (string)
//	X-Replay-Start-Date         → context.replayDateRange.startDate (string, validated as RFC3339)
//	X-Replay-End-Date           → context.replayDateRange.endDate   (string, validated as RFC3339)
//	X-Replay-Destination-Filter → context.replayDestinationFilter  (string)
//	X-Replay-Dry-Run            → context.replayDryRun             (bool)
//
// When none of the advanced replay headers are present the request passes through unchanged,
// maintaining full backward compatibility with existing replay requests.
//
// The middleware follows the identical body-read/modify/replace pattern established by
// withWarehouseReplayTag in handle_http_replay.go:
//  1. Read headers; short-circuit if none present
//  2. Read and close the request body via io.ReadAll
//  3. Detect batch vs single-event format via gjson.GetBytes(body, "batch")
//  4. Inject filter values into each event's context using sjson.SetBytes
//  5. Replace the request body with the modified payload
//  6. Delegate to the next handler in the chain
//
// The intended middleware chain for advanced replay is:
//
//	callType("replay") → replaySourceIDAuth → withAdvancedReplayFilters → withWarehouseReplayTag → webHandler
func (gw *Handle) withAdvancedReplayFilters(delegate http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract all advanced replay filter headers from the request.
		sourceFilter := r.Header.Get("X-Replay-Source-Filter")
		startDate := r.Header.Get("X-Replay-Start-Date")
		endDate := r.Header.Get("X-Replay-End-Date")
		destFilter := r.Header.Get("X-Replay-Destination-Filter")
		dryRunStr := r.Header.Get("X-Replay-Dry-Run")

		// If no advanced headers are present, pass through unchanged to preserve
		// backward compatibility with existing replay requests that only use the
		// base X-Warehouse-Replay header handled by withWarehouseReplayTag.
		if sourceFilter == "" && startDate == "" && endDate == "" && destFilter == "" && dryRunStr == "" {
			delegate.ServeHTTP(w, r)
			return
		}

		// Read the request body to inject advanced filter parameters.
		// Follow the same io.ReadAll + Close pattern as withWarehouseReplayTag (lines 47-48).
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil || len(body) == 0 {
			// On read error or empty body, pass through without modification.
			// The downstream handler (webHandler → getPayloadFromRequest) will
			// handle the error appropriately. Follow the same defensive pattern
			// as withWarehouseReplayTag (lines 49-57).
			if len(body) > 0 {
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			delegate.ServeHTTP(w, r)
			return
		}

		// Validate date-range parameters if provided.
		// Invalid dates are logged as warnings but still injected — the downstream
		// Processor/Archiver can apply stricter validation and reject if needed.
		if startDate != "" {
			if _, parseErr := time.Parse(time.RFC3339, startDate); parseErr != nil {
				gw.logger.Warnn("Invalid X-Replay-Start-Date format, expected RFC3339",
					logger.NewStringField("value", startDate))
			}
		}
		if endDate != "" {
			if _, parseErr := time.Parse(time.RFC3339, endDate); parseErr != nil {
				gw.logger.Warnn("Invalid X-Replay-End-Date format, expected RFC3339",
					logger.NewStringField("value", endDate))
			}
		}

		// Inject filter parameters into each event's context.
		// Replay requests use batch format: {"batch": [{...}, {...}]}
		// The "batch" array is the standard format for the replay callType
		// (see handle.go: reqType != "batch" && reqType != "replay" && reqType != "retl").
		// Follow the exact same batch/single-event detection pattern from
		// withWarehouseReplayTag (lines 64-77).
		batch := gjson.GetBytes(body, "batch")
		if batch.Exists() && batch.IsArray() {
			for i := range batch.Array() {
				prefix := "batch." + strconv.Itoa(i) + ".context."
				body = injectReplayFilters(body, prefix, sourceFilter, startDate, endDate, destFilter, dryRunStr)
			}
		} else {
			// Single event format (non-batch replay — defensive handling)
			body = injectReplayFilters(body, "context.", sourceFilter, startDate, endDate, destFilter, dryRunStr)
		}

		// Replace the request body with the modified payload.
		// Update ContentLength to match the new body size so downstream
		// handlers do not read a truncated or oversized body.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		delegate.ServeHTTP(w, r)
	}
}
