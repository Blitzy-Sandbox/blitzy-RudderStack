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
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-server/warehouse/replay"
)

// ---------------------------------------------------------------------------
// Mock ArchiverQuerier
// ---------------------------------------------------------------------------

// mockArchiverQuerier is a configurable test double for the replay.ArchiverQuerier
// interface. The queryFn field controls the return value.
type mockArchiverQuerier struct {
	queryFn func(ctx context.Context, sourceID string, start, end time.Time) ([]replay.ArchivedEventBatch, error)
}

// QueryArchivedEvents delegates to the configured queryFn.
func (m *mockArchiverQuerier) QueryArchivedEvents(
	ctx context.Context,
	sourceID string,
	startTime, endTime time.Time,
) ([]replay.ArchivedEventBatch, error) {
	return m.queryFn(ctx, sourceID, startTime, endTime)
}

// ---------------------------------------------------------------------------
// Helper: create gzip JSONL data from events
// ---------------------------------------------------------------------------

// gzipJSONL compresses a slice of ArchivedEvent into gzipped JSONL format
// suitable for use as ArchivedEventBatch.Data in test fixtures.
func gzipJSONL(t *testing.T, events []replay.ArchivedEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	for _, ev := range events {
		line, err := jsonrs.Marshal(ev)
		require.NoError(t, err, "failed to marshal event to JSON")
		_, err = gzWriter.Write(line)
		require.NoError(t, err, "failed to write line to gzip")
		_, err = gzWriter.Write([]byte("\n"))
		require.NoError(t, err, "failed to write newline to gzip")
	}
	require.NoError(t, gzWriter.Close(), "failed to close gzip writer")
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// TestDeserializeGzipJSONL
// ---------------------------------------------------------------------------

// TestDeserializeGzipJSONL exercises the exported DeserializeGzipJSONL utility
// function which decompresses gzipped JSONL data into ArchivedEvent slices.
func TestDeserializeGzipJSONL(t *testing.T) {
	sampleEvents := []replay.ArchivedEvent{
		{
			MessageID: "msg-001",
			Type:      "track",
			Event:     "Product Viewed",
			UserID:    "user-1",
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
			name: "valid gzip JSONL with multiple events",
			data: func() []byte {
				return gzipJSONL(t, sampleEvents)
			},
			wantCount: 2,
		},
		{
			name: "empty data returns nil",
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
			name: "invalid gzip data returns error",
			data: func() []byte {
				return []byte("this is not gzip data")
			},
			wantErr:    true,
			errContain: "creating gzip reader",
		},
		{
			name: "valid gzip with invalid JSON returns error",
			data: func() []byte {
				var buf bytes.Buffer
				gzWriter := gzip.NewWriter(&buf)
				_, err := gzWriter.Write([]byte("{invalid json}\n"))
				require.NoError(t, err)
				require.NoError(t, gzWriter.Close())
				return buf.Bytes()
			},
			wantErr:    true,
			errContain: "deserializing event",
		},
		{
			name: "gzip JSONL with empty lines are skipped",
			data: func() []byte {
				var buf bytes.Buffer
				gzWriter := gzip.NewWriter(&buf)
				line, _ := jsonrs.Marshal(sampleEvents[0])
				_, _ = gzWriter.Write(line)
				_, _ = gzWriter.Write([]byte("\n\n\n")) // extra empty lines
				line2, _ := jsonrs.Marshal(sampleEvents[1])
				_, _ = gzWriter.Write(line2)
				_, _ = gzWriter.Write([]byte("\n"))
				require.NoError(t, gzWriter.Close())
				return buf.Bytes()
			},
			wantCount: 2,
		},
		{
			name: "single event in gzip JSONL",
			data: func() []byte {
				return gzipJSONL(t, sampleEvents[:1])
			},
			wantCount: 1,
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
			require.Len(t, events, tc.wantCount)
			if tc.wantCount > 0 {
				require.Equal(t, sampleEvents[0].MessageID, events[0].MessageID)
				require.Equal(t, sampleEvents[0].Type, events[0].Type)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestArchivedEventRetriever_Retrieve
// ---------------------------------------------------------------------------

// TestArchivedEventRetriever_Retrieve exercises the Retrieve method through
// the mock ArchiverQuerier, covering: successful retrieval, empty results,
// archiver errors, and context cancellation.
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
		name       string
		archiver   *mockArchiverQuerier
		ctx        func() (context.Context, context.CancelFunc)
		wantCount  int
		wantErr    bool
		errContain string
	}{
		{
			name: "successful retrieval with one batch",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, sourceID string, start, end time.Time) ([]replay.ArchivedEventBatch, error) {
					require.Equal(t, "src1", sourceID)
					return []replay.ArchivedEventBatch{
						{
							SourceID:   "src1",
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
		},
		{
			name: "successful retrieval with multiple batches",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, _ string, start, end time.Time) ([]replay.ArchivedEventBatch, error) {
					return []replay.ArchivedEventBatch{
						{
							SourceID:   "src1",
							Data:       gzipJSONL(t, []replay.ArchivedEvent{sampleEvent}),
							StartTime:  start,
							EndTime:    end,
							EventCount: 1,
						},
						{
							SourceID:   "src1",
							Data:       gzipJSONL(t, []replay.ArchivedEvent{sampleEvent, sampleEvent}),
							StartTime:  start,
							EndTime:    end,
							EventCount: 2,
						},
					}, nil
				},
			},
			ctx:       func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantCount: 3,
		},
		{
			name: "empty result returns nil",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, _ string, _, _ time.Time) ([]replay.ArchivedEventBatch, error) {
					return nil, nil
				},
			},
			ctx:       func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			wantCount: 0,
		},
		{
			name: "archiver error propagated",
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
			name: "context cancellation during batch processing",
			archiver: &mockArchiverQuerier{
				queryFn: func(_ context.Context, _ string, start, end time.Time) ([]replay.ArchivedEventBatch, error) {
					// Return two batches so context cancellation is checked between them
					return []replay.ArchivedEventBatch{
						{
							SourceID:   "src1",
							Data:       gzipJSONL(t, []replay.ArchivedEvent{sampleEvent}),
							StartTime:  start,
							EndTime:    end,
							EventCount: 1,
						},
						{
							SourceID:   "src1",
							Data:       gzipJSONL(t, []replay.ArchivedEvent{sampleEvent}),
							StartTime:  start,
							EndTime:    end,
							EventCount: 1,
						},
					}, nil
				},
			},
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // Cancel immediately
				return ctx, cancel
			},
			wantErr:    true,
			errContain: "context cancelled",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()

			retriever := replay.NewArchivedEventRetriever(
				config.New(),
				logger.NOP,
				stats.NOP,
				tc.archiver,
			)

			events, err := retriever.Retrieve(ctx, "src1", startTime, endTime)
			if tc.wantErr {
				require.Error(t, err)
				if tc.errContain != "" {
					require.Contains(t, err.Error(), tc.errContain)
				}
				return
			}
			require.NoError(t, err)
			require.Len(t, events, tc.wantCount)
			if tc.wantCount > 0 {
				require.Equal(t, sampleEvent.MessageID, events[0].MessageID)
				require.Equal(t, sampleEvent.Type, events[0].Type)
				require.Equal(t, sampleEvent.Event, events[0].Event)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestBatchEvents
// ---------------------------------------------------------------------------

// TestBatchEvents exercises the batchEvents helper exported indirectly through
// the DeserializeGzipJSONL + retrieve pipeline. This tests the batching behavior
// by verifying that the retriever handles different batch configurations correctly
// through configuration.
func TestArchivedEventRetriever_MultipleEventsAcrossBatches(t *testing.T) {
	// Create 5 events across 2 archiver batches
	events1 := []replay.ArchivedEvent{
		{MessageID: "msg-1", Type: "track", ReceivedAt: time.Now().UTC()},
		{MessageID: "msg-2", Type: "track", ReceivedAt: time.Now().UTC()},
		{MessageID: "msg-3", Type: "identify", ReceivedAt: time.Now().UTC()},
	}
	events2 := []replay.ArchivedEvent{
		{MessageID: "msg-4", Type: "page", ReceivedAt: time.Now().UTC()},
		{MessageID: "msg-5", Type: "screen", ReceivedAt: time.Now().UTC()},
	}

	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	archiver := &mockArchiverQuerier{
		queryFn: func(_ context.Context, _ string, _, _ time.Time) ([]replay.ArchivedEventBatch, error) {
			return []replay.ArchivedEventBatch{
				{
					SourceID:   "src1",
					Data:       gzipJSONL(t, events1),
					StartTime:  start,
					EndTime:    end,
					EventCount: 3,
				},
				{
					SourceID:   "src1",
					Data:       gzipJSONL(t, events2),
					StartTime:  start,
					EndTime:    end,
					EventCount: 2,
				},
			}, nil
		},
	}

	retriever := replay.NewArchivedEventRetriever(
		config.New(),
		logger.NOP,
		stats.NOP,
		archiver,
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
}

// TestArchivedEventRetriever_EmptyBatches verifies that archiver returning
// an empty slice (not nil) is handled gracefully.
func TestArchivedEventRetriever_EmptyBatches(t *testing.T) {
	archiver := &mockArchiverQuerier{
		queryFn: func(_ context.Context, _ string, _, _ time.Time) ([]replay.ArchivedEventBatch, error) {
			return []replay.ArchivedEventBatch{}, nil
		},
	}

	retriever := replay.NewArchivedEventRetriever(
		config.New(),
		logger.NOP,
		stats.NOP,
		archiver,
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
