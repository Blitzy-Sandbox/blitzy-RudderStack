// Package replay provides the warehouse replay feature (E-035) for re-processing
// archived events through the warehouse pipeline, bypassing real-time Router delivery.
//
// This file implements HTTPGatewayClient, the concrete implementation of the
// GatewayClient interface that sends replay event batches to the Gateway's
// HTTP replay endpoint with the X-Warehouse-Replay header set to "true".
//
// The Gateway's webReplayHandler() (gateway/handle_http_replay.go) detects the
// X-Warehouse-Replay header and tags events for warehouse-only routing through
// the Processor pipeline, bypassing real-time Router delivery.
package replay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rudderlabs/rudder-go-kit/logger"
)

// HTTPGatewayClient implements the GatewayClient interface by sending replay
// event batches to the Gateway's HTTP replay endpoint via POST requests with
// the X-Warehouse-Replay header. This header triggers warehouse-only routing
// in the Processor pipeline.
//
// The gatewayURL is the full URL to the gateway replay endpoint, e.g.,
// "http://localhost:8080/internal/v1/replay". It is resolved at runtime from
// the Gateway.webPort configuration, since the Gateway port is not known until
// after Setup() completes.
type HTTPGatewayClient struct {
	gatewayURL string
	httpClient *http.Client
	log        logger.Logger
}

// NewHTTPGatewayClient creates a new HTTPGatewayClient that sends replay batches
// to the specified gateway replay URL. The URL should include the full path to the
// Gateway's replay endpoint (e.g., "http://localhost:8080/internal/v1/replay").
//
// A dedicated http.Client with a 30-second timeout is used to prevent indefinite
// blocking on slow or unresponsive Gateway endpoints.
func NewHTTPGatewayClient(gatewayURL string, log logger.Logger) *HTTPGatewayClient {
	return &HTTPGatewayClient{
		gatewayURL: gatewayURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		log: log.Child("replay.gatewayClient"),
	}
}

// SendReplayBatch sends a batch of replay events to the Gateway's replay endpoint.
// The request includes the X-Warehouse-Replay: true header to trigger warehouse-only
// routing in the Processor pipeline (gateway/handle_http_replay.go detects this header
// via withWarehouseReplayTag middleware).
//
// Returns an error if the HTTP request fails or the Gateway returns a non-2xx response.
func (c *HTTPGatewayClient) SendReplayBatch(ctx context.Context, batch []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.gatewayURL, bytes.NewReader(batch))
	if err != nil {
		return fmt.Errorf("creating replay request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(WarehouseReplayHeader, WarehouseReplayHeaderValue)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending replay batch to gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Drain the body to allow connection reuse by the HTTP transport pool.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gateway returned non-2xx status: %d", resp.StatusCode)
	}

	c.log.Debugn("replay batch sent to gateway successfully",
		logger.NewIntField("batchSize", int64(len(batch))),
		logger.NewIntField("statusCode", int64(resp.StatusCode)),
	)
	return nil
}
