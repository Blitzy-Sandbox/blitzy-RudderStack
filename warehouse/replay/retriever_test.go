// Package replay_test contains black-box unit tests for the ArchivedEventRetriever
// and its DeserializeGzipJSONL utility function.
//
// All tests follow the project's table-driven t.Run() + testify/require conventions.
// JSON uses github.com/rudderlabs/rudder-go-kit/jsonrs per .golangci.yml depguard.
package replay_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats/memstats"

	"github.com/rudderlabs/rudder-server/warehouse/replay"
)

// Compile-time assertions.
var _ replay.ArchiverQuerier = (*mockArchiverQuerier)(nil)
var _ replay.FileDownloader = (*noopFileDownloader)(nil)

// ---------------------------------------------------------------------------
// noopFileDownloader — used in tests where batches already have Data populated
// ---------------------------------------------------------------------------

// noopFileDownloader is a no-op FileDownloader that panics if called.
// It is used in tests where batch.Data is pre-populated, so the downloader
// should never be invoked. If it IS called, the panic provides a clear signal
// that the test setup is incorrect.
type noopFileDownloader struct{}

func (n *noopFileDownloader) Download(_ context.Context, location string) ([]byte, error) {
	panic("noopFileDownloader.Download called unexpectedly for location: " + location)
}

// ---------------------------------------------------------------------------
// Mock ArchiverQuerier
// ---------------------------------------------------------------------------

// mockArchiverQuerier is a configurable test double for the replay.ArchiverQuerier
// interface. The queryFn field controls the QueryArchivedEvents return value.
type mockArchiverQuerier struct {
	queryFn func(ctx context.Context, sourceID string, startTime, endTime time.Time) ([]replay.ArchivedEventBatch, error)
}

// QueryArchivedEvents delegates to the configured queryFn, or returns a default
// "not implemented" error if queryFn is nil.
func (m *mockArchiverQuerier) QueryArchivedEvents(
	ctx context.Context,
	sourceID string,
	startTime, endTime time.Time,
) ([]replay.ArchivedEventBatch, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, sourceID, startTime, endTime)
	}
	return nil, errors.New("not implemented")
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

// createGzipJSONL creates gzip-compressed JSONL data from a slice of generic
// event maps (map[string]interface{}). Uses jsonrs.Marshal per .golangci.yml
// depguard rules — NEVER encoding/json.
func createGzipJSONL(t *testing.T, events []map[string]interface{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, event := range events {
		line, err := jsonrs.Marshal(event)
		require.NoError(t, err)
		_, err = gz.Write(append(line, '\n'))
		require.NoError(t, err)
	}
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// gzipJSONL creates gzip-compressed JSONL data from typed ArchivedEvent slices.
// This is the primary helper for constructing ArchivedEventBatch.Data in test
// fixtures that use strongly-typed event models.
func gzipJSONL(t *testing.T, events []replay.ArchivedEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, ev := range events {
		line, err := jsonrs.Marshal(ev)
		require.NoError(t, err)
		_, err = gz.Write(line)
		require.NoError(t, err)
		_, err = gz.Write([]byte("\n"))
		require.NoError(t, err)
	}
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// sampleEvents returns canonical event map fixtures with known messageId and
// type values for use in createGzipJSONL-based test scenarios.
func sampleEvents() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"messageId":  "msg-001",
			"type":       "track",
			"event":      "Product Viewed",
			"userId":     "user-123",
			"receivedAt": "2024-01-01T00:00:00.000Z",
		},
		{
			"messageId":  "msg-002",
			"type":       "identify",
			"userId":     "user-456",
			"receivedAt": "2024-01-01T01:00:00.000Z",
		},
	}
}

// testConfig creates a config.Config instance pre-populated with standard
// replay configuration using the exported config key constants from the
// replay package.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	conf := config.New()
	conf.Set(replay.ConfigKeyEnabled, true)
	conf.Set(replay.ConfigKeyBatchSize, 1000)
	conf.Set(replay.ConfigKeyMaxConcurrentReplays, 2)
	conf.Set(replay.ConfigKeyTimeoutMinutes, 60)
	return conf
}

// batchEvents splits a flat slice of ArchivedEvent into sub-slices of at most
// batchSize elements. This helper mirrors the batching logic used by the replay
// handler when preparing retriever output for Gateway injection.
func batchEvents(events []replay.ArchivedEvent, batchSize int) [][]replay.ArchivedEvent {
	if len(events) == 0 || batchSize <= 0 {
		return nil
	}
	var batches [][]replay.ArchivedEvent
	for i := 0; i < len(events); i += batchSize {
		end := i + batchSize
		if end > len(events) {
			end = len(events)
		}
		batches = append(batches, events[i:end])
	}
	return batches
}

// newTestStats creates an in-memory stats store suitable for test use.
// The returned memstats.Store implements stats.Stats and captures all emitted
// metrics for optional inspection. Follows the pattern from
// warehouse/router/tracker_test.go.
func newTestStats(t *testing.T) *memstats.Store {
	t.Helper()
	store, err := memstats.New()
	require.NoError(t, err)
	return store
}

// makeEvents creates n deterministic ArchivedEvent values for batch/size tests.
// All events use the same MessageID and Type for simplicity; individual
// identification is not needed for count-based assertions.
func makeEvents(n int) []replay.ArchivedEvent {
	events := make([]replay.ArchivedEvent, n)
	for i := range events {
		events[i] = replay.ArchivedEvent{
			MessageID:  "msg",
			Type:       "track",
			ReceivedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	return events
}

// ---------------------------------------------------------------------------
// TestDeserializeGzipJSONL
// ---------------------------------------------------------------------------

// TestDeserializeGzipJSONL exercises the exported DeserializeGzipJSONL utility
// function which decompresses gzipped JSONL data into ArchivedEvent slices.
// Covers: valid data, empty data, invalid gzip, invalid JSON, and empty lines.
func TestDeserializeGzipJSONL(t *testing.T) {
	typedEvents := []replay.ArchivedEvent{
		{
			MessageID:  "msg-001",
			Type:       "track",
			Event:      "Product Viewed",
			UserID:     "user-1",
			ReceivedAt: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
		},
		{
			MessageID:   "msg-002",
			Type:        "identify",
			UserID:      "user-2",
			AnonymousID: "anon-1",
			ReceivedAt:  time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC),
		},
	}

	tests := []struct {
		name       string
		data       func() []byte
		wantCount  int
		wantErr    bool
		errContain string
	}{
		{
			name: "valid gzip JSONL deserialization",
			data: func() []byte {
				return gzipJSONL(t, typedEvents)
			},
			wantCount: 2,
		},
		{
			name: "valid gzip JSONL from map events",
			data: func() []byte {
				return createGzipJSONL(t, sampleEvents())
			},
			wantCount: 2,
		},
		{
			name: "single event in gzip JSONL",
			data: func() []byte {
				return gzipJSONL(t, typedEvents[:1])
			},
			wantCount: 1,
		},
		{
			name: "invalid gzip data returns error",
			data: func() []byte {
				return []byte("this is not gzip data")
			},
			wantErr:    true,
			errContain: "creating gzip reader",
		},
		{
			name: "invalid JSON line returns error",
			data: func() []byte {
				var buf bytes.Buffer
				gz := gzip.NewWriter(&buf)
				_, err := gz.Write([]byte("{invalid json}\n"))
				require.NoError(t, err)
				require.NoError(t, gz.Close())
				return buf.Bytes()
			},
			wantErr:    true,
			errContain: "deserializing event",
		},
		{
			name: "empty gzip data returns empty events",
			data: func() []byte {
				return nil
			},
			wantCount: 0,
		},
		{
			name: "empty byte slice returns nil",
			data: func() []byte {
				return []byte{}
			},
			wantCount: 0,
		},
		{
			name: "gzip JSONL with empty lines are skipped",
			data: func() []byte {
				var buf bytes.Buffer
				gz := gzip.NewWriter(&buf)
				line, _ := jsonrs.Marshal(typedEvents[0])
				_, _ = gz.Write(line)
				_, _ = gz.Write([]byte("\n\n\n")) // extra empty lines
				line2, _ := jsonrs.Marshal(typedEvents[1])
				_, _ = gz.Write(line2)
				_, _ = gz.Write([]byte("\n"))
				require.NoError(t, gz.Close())
				return buf.Bytes()
			},
			wantCount: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := replay.DeserializeGzipJSONL(tc.data())
			if tc.wantErr {
				require.Error(t, err)
				if tc.errContain != "" {
					require.Contains(t, err.Error(), tc.errContain)
				}
				return
			}
			require.NoError(t, err)
			if tc.wantCount == 0 {
				require.Nil(t, events)
			} else {
				require.Len(t, events, tc.wantCount)
				require.NotEmpty(t, events[0].MessageID)
				require.NotEmpty(t, events[0].Type)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestArchivedEventRetriever_Retrieve
// ---------------------------------------------------------------------------

// TestArchivedEventRetriever_Retrieve exercises the Retrieve method through
// the mock ArchiverQuerier, covering: successful retrieval (single/multiple
// batches), empty results, date filtering, archiver errors, and context
// cancellation.
func TestArchivedEventRetriever_Retrieve(t *testing.T) {
	startTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	endTime := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	sampleEvent := replay.ArchivedEvent{
		MessageID:  "msg-101",
		Type:       "track",
		Event:      "Order Completed",
		UserID:     "user-42",
		ReceivedAt: time.Date(2024, 1, 1, 15, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		name        string
		sourceID    string
		archiver    *mockArchiverQuerier
		ctx         func() (context.Context, context.CancelFunc)
		wantCount   int
		wantErr     bool
		errContain  string
		wantErrorIs error
		verify      func(t *testing.T, events []replay.ArchivedEvent)
	}{
		{
			name:     "successful retrieval with single batch",
			sourceID: "src1",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, sourceID string, start, end time.Time) ([]replay.ArchivedEventBatch, error) {
					require.Equal(t, "src1", sourceID)
					events := []replay.ArchivedEvent{
						sampleEvent,
						{MessageID: "msg-102", Type: "identify", ReceivedAt: time.Date(2024, 1, 1, 16, 0, 0, 0, time.UTC)},
						{MessageID: "msg-103", Type: "page", ReceivedAt: time.Date(2024, 1, 1, 17, 0, 0, 0, time.UTC)},
						{MessageID: "msg-104", Type: "screen", ReceivedAt: time.Date(2024, 1, 1, 18, 0, 0, 0, time.UTC)},
						{MessageID: "msg-105", Type: "group", ReceivedAt: time.Date(2024, 1, 1, 19, 0, 0, 0, time.UTC)},
					}
					return []replay.ArchivedEventBatch{
						{
							SourceID:   "src1",
							Data:       gzipJSONL(t, events),
							StartTime:  start,
							EndTime:    end,
							EventCount: 5,
						},
					}, nil
				},
			},
			ctx:       func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantCount: 5,
			verify: func(t *testing.T, events []replay.ArchivedEvent) {
				require.NotNil(t, events)
				require.Equal(t, sampleEvent.MessageID, events[0].MessageID)
				require.Equal(t, sampleEvent.Type, events[0].Type)
				require.Equal(t, sampleEvent.Event, events[0].Event)
			},
		},
		{
			name:     "successful retrieval with multiple batches",
			sourceID: "src1",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, _ string, start, end time.Time) ([]replay.ArchivedEventBatch, error) {
					batch1 := makeEvents(10)
					batch2 := makeEvents(5)
					batch3 := makeEvents(8)
					return []replay.ArchivedEventBatch{
						{SourceID: "src1", Data: gzipJSONL(t, batch1), StartTime: start, EndTime: end, EventCount: 10},
						{SourceID: "src1", Data: gzipJSONL(t, batch2), StartTime: start, EndTime: end, EventCount: 5},
						{SourceID: "src1", Data: gzipJSONL(t, batch3), StartTime: start, EndTime: end, EventCount: 8},
					}, nil
				},
			},
			ctx:       func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantCount: 23,
		},
		{
			name:     "empty result returns nil",
			sourceID: "src1",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, _ string, _, _ time.Time) ([]replay.ArchivedEventBatch, error) {
					return nil, nil
				},
			},
			ctx:       func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantCount: 0,
		},
		{
			name:     "date filtering respects time range",
			sourceID: "src-filter",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, sourceID string, start, end time.Time) ([]replay.ArchivedEventBatch, error) {
					// Verify the exact parameters passed to the archiver querier
					require.Equal(t, "src-filter", sourceID)
					require.Equal(t, startTime, start)
					require.Equal(t, endTime, end)
					return []replay.ArchivedEventBatch{
						{
							SourceID:   sourceID,
							Data:       gzipJSONL(t, []replay.ArchivedEvent{sampleEvent}),
							StartTime:  start,
							EndTime:    end,
							EventCount: 1,
						},
					}, nil
				},
			},
			ctx:       func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantCount: 1,
			verify: func(t *testing.T, events []replay.ArchivedEvent) {
				require.Equal(t, sampleEvent.MessageID, events[0].MessageID)
			},
		},
		{
			name:     "error from archiver propagates",
			sourceID: "src1",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, _ string, _, _ time.Time) ([]replay.ArchivedEventBatch, error) {
					return nil, errors.New("archiver connection failed")
				},
			},
			ctx:        func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantErr:    true,
			errContain: "querying archived events",
		},
		{
			name:     "context cancellation",
			sourceID: "src1",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, _ string, start, end time.Time) ([]replay.ArchivedEventBatch, error) {
					// Return two batches so context cancellation is checked between them
					return []replay.ArchivedEventBatch{
						{SourceID: "src1", Data: gzipJSONL(t, []replay.ArchivedEvent{sampleEvent}), StartTime: start, EndTime: end, EventCount: 1},
						{SourceID: "src1", Data: gzipJSONL(t, []replay.ArchivedEvent{sampleEvent}), StartTime: start, EndTime: end, EventCount: 1},
					}, nil
				},
			},
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // Cancel immediately before Retrieve is called
				return ctx, cancel
			},
			wantErr:     true,
			errContain:  "context cancelled",
			wantErrorIs: context.Canceled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()

			retriever := replay.NewArchivedEventRetriever(
				testConfig(t),
				logger.NOP,
				newTestStats(t),
				tc.archiver,
				&noopFileDownloader{},
			)

			events, err := retriever.Retrieve(ctx, tc.sourceID, startTime, endTime)
			if tc.wantErr {
				require.Error(t, err)
				if tc.errContain != "" {
					require.Contains(t, err.Error(), tc.errContain)
				}
				if tc.wantErrorIs != nil {
					require.ErrorIs(t, err, tc.wantErrorIs)
				}
				return
			}
			require.NoError(t, err)
			if tc.wantCount == 0 {
				require.Nil(t, events)
			} else {
				require.Len(t, events, tc.wantCount)
			}
			if tc.verify != nil {
				tc.verify(t, events)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestArchivedEventRetriever_Batch
// ---------------------------------------------------------------------------

// TestArchivedEventRetriever_Batch verifies the event batching logic that splits
// a flat slice of events into sub-slices of configurable batch size. This mirrors
// the batching behavior used by the ReplayHandler before Gateway injection.
func TestArchivedEventRetriever_Batch(t *testing.T) {
	tests := []struct {
		name              string
		eventCount        int
		batchSize         int
		wantBatchCount    int
		wantLastBatchSize int
	}{
		{
			name:              "batching splits events correctly",
			eventCount:        25,
			batchSize:         10,
			wantBatchCount:    3,
			wantLastBatchSize: 5,
		},
		{
			name:              "batch size of 1",
			eventCount:        3,
			batchSize:         1,
			wantBatchCount:    3,
			wantLastBatchSize: 1,
		},
		{
			name:              "batch size larger than events",
			eventCount:        5,
			batchSize:         100,
			wantBatchCount:    1,
			wantLastBatchSize: 5,
		},
		{
			name:              "empty events returns empty batches",
			eventCount:        0,
			batchSize:         10,
			wantBatchCount:    0,
			wantLastBatchSize: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := makeEvents(tc.eventCount)
			batches := batchEvents(events, tc.batchSize)

			if tc.wantBatchCount == 0 {
				require.Nil(t, batches)
				return
			}

			require.Len(t, batches, tc.wantBatchCount)
			require.Len(t, batches[len(batches)-1], tc.wantLastBatchSize)

			// Verify all non-last batches are at full capacity
			for i := 0; i < len(batches)-1; i++ {
				require.Len(t, batches[i], tc.batchSize)
			}

			// Verify total event count across all batches equals input
			totalEvents := 0
			for _, batch := range batches {
				totalEvents += len(batch)
			}
			require.Equal(t, tc.eventCount, totalEvents)
		})
	}
}

// ---------------------------------------------------------------------------
// TestArchivedEventRetriever_MultipleEventsAcrossBatches
// ---------------------------------------------------------------------------

// TestArchivedEventRetriever_MultipleEventsAcrossBatches verifies that events
// from multiple archiver batches are correctly merged into a flat list while
// preserving the original ordering (batch 1 events, then batch 2 events).
func TestArchivedEventRetriever_MultipleEventsAcrossBatches(t *testing.T) {
	events1 := []replay.ArchivedEvent{
		{MessageID: "msg-1", Type: "track", ReceivedAt: time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)},
		{MessageID: "msg-2", Type: "track", ReceivedAt: time.Date(2024, 1, 1, 2, 0, 0, 0, time.UTC)},
		{MessageID: "msg-3", Type: "identify", ReceivedAt: time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC)},
	}
	events2 := []replay.ArchivedEvent{
		{MessageID: "msg-4", Type: "page", ReceivedAt: time.Date(2024, 1, 1, 4, 0, 0, 0, time.UTC)},
		{MessageID: "msg-5", Type: "screen", ReceivedAt: time.Date(2024, 1, 1, 5, 0, 0, 0, time.UTC)},
	}

	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	archiver := &mockArchiverQuerier{
		queryFn: func(_ context.Context, _ string, _, _ time.Time) ([]replay.ArchivedEventBatch, error) {
			return []replay.ArchivedEventBatch{
				{SourceID: "src1", Data: gzipJSONL(t, events1), StartTime: start, EndTime: end, EventCount: 3},
				{SourceID: "src1", Data: gzipJSONL(t, events2), StartTime: start, EndTime: end, EventCount: 2},
			}, nil
		},
	}

	retriever := replay.NewArchivedEventRetriever(
		testConfig(t),
		logger.NOP,
		newTestStats(t),
		archiver,
		&noopFileDownloader{},
	)

	result, err := retriever.Retrieve(context.Background(), "src1", start, end)
	require.NoError(t, err)
	require.Len(t, result, 5)

	// Verify event ordering matches input batch ordering
	require.Equal(t, "msg-1", result[0].MessageID)
	require.Equal(t, "msg-2", result[1].MessageID)
	require.Equal(t, "msg-3", result[2].MessageID)
	require.Equal(t, "msg-4", result[3].MessageID)
	require.Equal(t, "msg-5", result[4].MessageID)

	// Verify event types are preserved
	require.Equal(t, "track", result[0].Type)
	require.Equal(t, "identify", result[2].Type)
	require.Equal(t, "page", result[3].Type)
	require.Equal(t, "screen", result[4].Type)
}

// ---------------------------------------------------------------------------
// TestArchivedEventRetriever_EmptyBatches
// ---------------------------------------------------------------------------

// TestArchivedEventRetriever_EmptyBatches verifies that an archiver returning
// an empty slice (not nil) is handled gracefully: no events, no error.
func TestArchivedEventRetriever_EmptyBatches(t *testing.T) {
	archiver := &mockArchiverQuerier{
		queryFn: func(_ context.Context, _ string, _, _ time.Time) ([]replay.ArchivedEventBatch, error) {
			return []replay.ArchivedEventBatch{}, nil
		},
	}

	retriever := replay.NewArchivedEventRetriever(
		testConfig(t),
		logger.NOP,
		newTestStats(t),
		archiver,
		&noopFileDownloader{},
	)

	result, err := retriever.Retrieve(
		context.Background(),
		"src1",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	require.Nil(t, result, "empty batch list should return nil events")
}

// ---------------------------------------------------------------------------
// TestArchivedEventRetriever_DeserializationError
// ---------------------------------------------------------------------------

// TestArchivedEventRetriever_DeserializationError verifies that when the
// archiver returns batches with corrupt gzip data, the Retrieve method
// returns an appropriate error wrapping the deserialization failure.
func TestArchivedEventRetriever_DeserializationError(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	archiver := &mockArchiverQuerier{
		queryFn: func(_ context.Context, _ string, _, _ time.Time) ([]replay.ArchivedEventBatch, error) {
			return []replay.ArchivedEventBatch{
				{
					SourceID:   "src1",
					Data:       []byte("corrupt data that is not gzip"),
					StartTime:  start,
					EndTime:    end,
					EventCount: 1,
				},
			}, nil
		},
	}

	retriever := replay.NewArchivedEventRetriever(
		testConfig(t),
		logger.NOP,
		newTestStats(t),
		archiver,
		&noopFileDownloader{},
	)

	result, err := retriever.Retrieve(context.Background(), "src1", start, end)
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, err.Error(), "deserializing batch")
}
