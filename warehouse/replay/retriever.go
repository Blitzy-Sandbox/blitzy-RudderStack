// Package replay provides the warehouse replay feature (E-035) for re-processing
// archived events through the warehouse pipeline.
//
// This file implements ArchivedEventRetriever, which queries the archiver for
// gateway events within a date range, deserializes them from the archived gzip
// JSONL format, and prepares batches for replay injection into the Gateway.
//
// The retriever follows the struct pattern from warehouse/archive/archiver.go
// (config, logger, stats fields) and the data model patterns from
// warehouse/internal/model/staging.go.
package replay

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
)

// ArchiverQuerier provides access to archived gateway events for replay.
// This interface abstracts the archiver's query capability, enabling the
// ArchivedEventRetriever to fetch event batches without depending on the
// concrete archiver implementation.
//
// The interface is defined here because it is the primary dependency of
// ArchivedEventRetriever. If handler.go also defines this interface, the
// duplicate must be removed during module validation.
type ArchiverQuerier interface {
	// QueryArchivedEvents returns archived gateway event payloads for the specified
	// source within the given time range. Each returned ArchivedEventBatch contains
	// gzip-compressed JSONL data that can be decompressed and parsed into individual
	// ArchivedEvent structs.
	QueryArchivedEvents(
		ctx context.Context,
		sourceID string,
		startTime, endTime time.Time,
	) ([]ArchivedEventBatch, error)
}

// maxTokenSize is the maximum size of a single JSONL line in bytes (10 MB).
// This accommodates very large gateway event payloads while preventing
// unbounded memory allocation from malformed data.
const maxTokenSize = 10 * 1024 * 1024

// initialBufSize is the initial buffer capacity for the JSONL line scanner (64 KB).
// The scanner will grow up to maxTokenSize as needed.
const initialBufSize = 64 * 1024

// ArchivedEventRetriever queries the archiver for gateway events within a date range,
// deserializes them from archived gzip JSONL format, and prepares batches for replay.
//
// It operates in the following stages:
//  1. Query ArchiverQuerier for archived event batches matching the source and date range
//  2. For each batch, decompress gzip-compressed JSONL data
//  3. Parse each JSONL line into an ArchivedEvent
//  4. Return all events as a flat slice for downstream batching by the ReplayHandler
//
// The retriever emits Prometheus counters for events retrieved and retrieval errors,
// following the stats pattern established in warehouse/router/upload_stats.go.
type ArchivedEventRetriever struct {
	conf         *config.Config
	log          logger.Logger
	statsFactory stats.Stats
	archiver     ArchiverQuerier

	config struct {
		batchSize config.ValueLoader[int]
	}

	// Stats counters for Prometheus metrics
	eventsRetrieved stats.Counter
	retrievalErrors stats.Counter
}

// NewArchivedEventRetriever creates a new ArchivedEventRetriever with the provided
// dependencies. The archiver parameter provides access to archived events via the
// ArchiverQuerier interface (defined in handler.go).
//
// Configuration is loaded using the reloadable config pattern from
// warehouse/archive/archiver.go (lines 92-98), enabling runtime reconfiguration
// without process restart.
//
// Parameters:
//   - conf: Configuration instance for loading reloadable parameters
//   - log: Structured logger for diagnostic output
//   - statsFactory: Prometheus-compatible stats factory for metric emission
//   - archiver: Interface for querying archived gateway events
func NewArchivedEventRetriever(
	conf *config.Config,
	log logger.Logger,
	statsFactory stats.Stats,
	archiver ArchiverQuerier,
) *ArchivedEventRetriever {
	r := &ArchivedEventRetriever{
		conf:         conf,
		log:          log.Child("replay.retriever"),
		statsFactory: statsFactory,
		archiver:     archiver,
	}

	// Load reloadable configuration — follows warehouse/archive/archiver.go pattern (lines 92-98).
	// The batchSize controls how many events are grouped per batch when returned;
	// it can be changed at runtime via config hot-reload without restart.
	r.config.batchSize = conf.GetReloadableIntVar(
		DefaultBatchSize, 1,
		ConfigKeyBatchSize,
	)

	// Stats counters — follows warehouse/archive/archiver.go pattern (line 100).
	// These counters track cumulative events retrieved and errors for Prometheus scraping.
	r.eventsRetrieved = statsFactory.NewStat(
		"warehouse.replay.eventsRetrieved", stats.CountType,
	)
	r.retrievalErrors = statsFactory.NewStat(
		"warehouse.replay.retrievalErrors", stats.CountType,
	)

	return r
}

// Retrieve queries the archiver for all gateway events within the specified date range
// for the given source, deserializes them from gzip JSONL format, and returns them
// as a flat slice of ArchivedEvent.
//
// The retrieval flow is:
//  1. Query ArchiverQuerier for archived event batches matching sourceID and time range
//  2. For each batch, decompress gzip JSONL data via deserializeBatch
//  3. Parse each JSONL line into an ArchivedEvent using jsonrs.Unmarshal
//  4. Return all events as a flat slice
//
// The method checks for context cancellation between batch iterations, enabling
// graceful shutdown during long retrieval operations. On cancellation, it returns
// a wrapped context error with progress indication.
//
// On success, the eventsRetrieved counter is incremented by the total event count.
// On failure, the retrievalErrors counter is incremented.
//
// Parameters:
//   - ctx: Context for cancellation and timeout support
//   - sourceID: The source identifier to query archived events for
//   - startTime: Start of the time range (inclusive)
//   - endTime: End of the time range (inclusive)
//
// Returns:
//   - A slice of deserialized ArchivedEvent, or nil if no events found
//   - An error if the archiver query or deserialization fails
func (r *ArchivedEventRetriever) Retrieve(
	ctx context.Context,
	sourceID string,
	startTime, endTime time.Time,
) ([]ArchivedEvent, error) {
	r.log.Infon("retrieving archived events",
		logger.NewStringField("sourceID", sourceID),
		logger.NewStringField("startTime", startTime.Format(time.RFC3339)),
		logger.NewStringField("endTime", endTime.Format(time.RFC3339)),
	)

	// 1. Query archiver for batches matching the source and date range
	batches, err := r.archiver.QueryArchivedEvents(ctx, sourceID, startTime, endTime)
	if err != nil {
		r.retrievalErrors.Increment()
		return nil, fmt.Errorf("querying archived events: %w", err)
	}

	if len(batches) == 0 {
		r.log.Infon("no archived event batches found",
			logger.NewStringField("sourceID", sourceID),
		)
		return nil, nil
	}

	// 2. Deserialize all batches, checking for cancellation between each
	var allEvents []ArchivedEvent
	for i, batch := range batches {
		// Check for context cancellation between batch iterations to support
		// graceful shutdown during long retrieval operations.
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf(
				"context cancelled during batch %d/%d: %w",
				i+1, len(batches), ctx.Err(),
			)
		default:
		}

		events, err := r.deserializeBatch(batch)
		if err != nil {
			r.retrievalErrors.Increment()
			return nil, fmt.Errorf("deserializing batch %d/%d: %w", i+1, len(batches), err)
		}
		allEvents = append(allEvents, events...)
	}

	// 3. Record successful retrieval metrics
	r.eventsRetrieved.Count(len(allEvents))
	r.log.Infon("archived events retrieved successfully",
		logger.NewIntField("totalEvents", int64(len(allEvents))),
		logger.NewIntField("totalBatches", int64(len(batches))),
	)

	return allEvents, nil
}

// deserializeBatch decompresses a gzipped JSONL batch and parses each line into
// an ArchivedEvent. This follows the same format the archiver uses to store events
// (see warehouse/archive/archiver.go lines 143-172 where TableJSONArchiver creates
// gzipped JSON output).
//
// The function:
//  1. Creates a gzip reader from the batch's compressed data
//  2. Uses a bufio.Scanner to read lines, with a configurable buffer up to maxTokenSize (10 MB)
//  3. Deserializes each non-empty line using jsonrs.Unmarshal (NOT encoding/json)
//
// Empty lines are silently skipped. If any line fails to deserialize, the function
// returns an error with the line number for debugging.
func (r *ArchivedEventRetriever) deserializeBatch(batch ArchivedEventBatch) ([]ArchivedEvent, error) {
	if len(batch.Data) == 0 {
		return nil, nil
	}

	// 1. Decompress gzip data from the batch
	gzReader, err := gzip.NewReader(bytes.NewReader(batch.Data))
	if err != nil {
		return nil, fmt.Errorf("creating gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	// 2. Read line by line (JSONL format — one JSON object per line)
	var events []ArchivedEvent
	scanner := bufio.NewScanner(gzReader)

	// Increase scanner buffer for large event payloads.
	// Default scanner buffer is 64KB; archived events can be much larger.
	scanner.Buffer(make([]byte, 0, initialBufSize), maxTokenSize)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue // skip empty lines between JSON objects
		}

		var event ArchivedEvent
		if err := jsonrs.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("deserializing event at line %d: %w", lineNum, err)
		}
		events = append(events, event)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning JSONL lines: %w", err)
	}

	return events, nil
}

// DeserializeGzipJSONL decompresses gzipped JSONL data and returns individual event
// payloads as a slice of ArchivedEvent. This is an exported utility function intended
// for use in testing and by other components that need to process archived event data
// outside the full retrieval pipeline.
//
// The function follows the same decompression and parsing logic as the internal
// deserializeBatch method, but operates on raw byte data rather than an
// ArchivedEventBatch struct.
//
// Parameters:
//   - data: Gzip-compressed JSONL bytes (one JSON object per line)
//
// Returns:
//   - A slice of deserialized ArchivedEvent, or nil if the input is empty
//   - An error if gzip decompression or JSON deserialization fails
func DeserializeGzipJSONL(data []byte) ([]ArchivedEvent, error) {
	if len(data) == 0 {
		return nil, nil
	}

	gzReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("creating gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	var events []ArchivedEvent
	scanner := bufio.NewScanner(gzReader)
	scanner.Buffer(make([]byte, 0, initialBufSize), maxTokenSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event ArchivedEvent
		if err := jsonrs.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("deserializing event: %w", err)
		}
		events = append(events, event)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning JSONL: %w", err)
	}

	return events, nil
}
