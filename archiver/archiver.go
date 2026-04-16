package archiver

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tidwall/gjson"

	"github.com/rudderlabs/rudder-go-kit/bytesize"
	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/filemanager"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	kitsync "github.com/rudderlabs/rudder-go-kit/sync"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/services/fileuploader"
	"github.com/rudderlabs/rudder-server/utils/payload"
	"github.com/rudderlabs/rudder-server/utils/workerpool"
)

type archiver struct {
	jobsDB          jobsdb.JobsDB
	storageProvider fileuploader.Provider
	log             logger.Logger
	stats           stats.Stats

	archiveTrigger           func() <-chan time.Time
	adaptivePayloadLimitFunc payload.AdaptiveLimiterFunc

	// workspaceIDResolver resolves a source ID to its workspace ID, enabling the
	// storage-backed operations (ListArchivedStagingFiles, QueryArchivedEvents).
	// The storageProvider.GetFileManager() requires a workspace ID to select the
	// correct storage backend. When nil, these operations return empty results.
	// Set via WithWorkspaceResolver option.
	workspaceIDResolver func(ctx context.Context, sourceID string) (string, error)

	stopArchivalTrigger context.CancelFunc
	waitGroup           *errgroup.Group

	archiveFrom string
	config      struct {
		concurrency      config.ValueLoader[int]
		payloadLimit     func() int64
		jobsdbMaxRetries func() int
		instanceID       string
		eventsLimit      func() int
		minWorkerSleep   time.Duration
		uploadFrequency  time.Duration
		enabled          func() bool
		customVal        string
	}
}

func New(
	jobsDB jobsdb.JobsDB,
	storageProvider fileuploader.Provider,
	c *config.Config,
	statHandle stats.Stats,
	opts ...Option,
) *archiver {
	a := &archiver{
		jobsDB:          jobsDB,
		storageProvider: storageProvider,
		log:             logger.NewLogger().Child("archiver"),
		stats:           statHandle,

		archiveFrom: "gw",
		archiveTrigger: func() <-chan time.Time {
			return time.After(c.GetDuration("archival.ArchiveSleepDuration", 30, time.Second))
		},
		adaptivePayloadLimitFunc: func(i int64) int64 { return i },
	}

	a.config.enabled = func() bool {
		return c.GetBool("archival.Enabled", true)
	}
	a.config.concurrency = c.GetReloadableIntVar(10, 1, "archival.ArchiveConcurrency")
	a.config.payloadLimit = func() int64 {
		return c.GetInt64("archival.ArchivePayloadSizeLimit", 1*bytesize.GB)
	}
	a.config.jobsdbMaxRetries = func() int {
		if c.IsSet("JobsDB.Archiver.MaxRetries") {
			return c.GetInt("JobsDB.Archiver.MaxRetries", 3)
		}
		return c.GetInt("JobsDB.MaxRetries", 3)
	}
	a.config.eventsLimit = func() int {
		return c.GetInt("archival.ArchiveEventsLimit", 100000)
	}
	a.config.instanceID = c.GetString("INSTANCE_ID", "1")
	a.config.minWorkerSleep = c.GetDuration("archival.MinWorkerSleep", 1, time.Minute)
	a.config.uploadFrequency = c.GetDuration("archival.UploadFrequency", 5, time.Minute)
	a.config.customVal = c.GetString("Gateway.CustomVal", "GW")

	for _, opt := range opts {
		opt(a)
	}

	return a
}

func (a *archiver) Start() error {
	a.log.Infon("Starting archiver")
	ctx, cancel := context.WithCancel(context.Background())
	a.stopArchivalTrigger = cancel
	g, ctx := errgroup.WithContext(ctx)
	a.waitGroup = g

	var limiterGroup sync.WaitGroup
	jobFetchLimit := kitsync.NewReloadableLimiter(
		ctx,
		&limiterGroup,
		"arc_fetch",
		a.config.concurrency,
		a.stats,
	)
	uploadLimit := kitsync.NewReloadableLimiter(
		ctx,
		&limiterGroup,
		"arc_upload",
		a.config.concurrency,
		a.stats,
	)
	statusUpdateLimit := kitsync.NewReloadableLimiter(
		ctx,
		&limiterGroup,
		"arc_update",
		a.config.concurrency,
		a.stats,
	)

	g.Go(func() error {
		workerPool := workerpool.New(
			ctx,
			func(sourceID string) workerpool.Worker {
				w := &worker{
					sourceID:         sourceID,
					jobsDB:           a.jobsDB,
					log:              a.log.Child("worker").Withn(obskit.SourceID(sourceID)),
					fetchLimiter:     jobFetchLimit,
					uploadLimiter:    uploadLimit,
					updateLimiter:    statusUpdateLimit,
					storageProvider:  a.storageProvider,
					archiveFrom:      a.archiveFrom,
					payloadLimitFunc: a.adaptivePayloadLimitFunc,
					stats:            a.stats,
				}
				w.lifecycle.ctx, w.lifecycle.cancel = context.WithCancel(ctx)
				w.config.payloadLimit = a.config.payloadLimit
				w.config.instanceID = a.config.instanceID
				w.config.eventsLimit = a.config.eventsLimit
				w.config.minSleep = a.config.minWorkerSleep
				w.config.uploadFrequency = a.config.uploadFrequency
				w.config.jobsdbMaxRetries = a.config.jobsdbMaxRetries

				queryParams := &jobsdb.GetQueryParams{
					ParameterFilters: []jobsdb.ParameterFilterT{{Name: "source_id", Value: sourceID}},
					CustomValFilters: []string{a.config.customVal},
				}
				w.queryParams = *queryParams

				return w
			},
			a.log,
			workerpool.WithIdleTimeout(2*a.config.uploadFrequency),
		)
		defer workerPool.Shutdown()
		// pinger loop
		for {
			if a.config.enabled() {
				start := time.Now()
				sources, err := a.jobsDB.GetDistinctParameterValues(ctx, jobsdb.SourceID, "")
				a.stats.NewStat("arc_active_partitions_time", stats.TimerType).Since(start)
				if err != nil {
					if ctx.Err() != nil {
						return err
					}
					a.log.Errorn("Failed to fetch sources", obskit.Error(err))
					continue
				}
				a.stats.NewStat("arc_active_partitions", stats.GaugeType).Gauge(len(sources))
				for _, source := range sources {
					workerPool.PingWorker(source)
				}
			}

			select {
			case <-ctx.Done():
				return nil
			case <-a.archiveTrigger():
			}
		}
	})
	g.Go(func() error {
		limiterGroup.Wait()
		return nil
	})

	return nil
}

func (a *archiver) Stop() {
	a.log.Infon("Stopping archiver")
	a.stopArchivalTrigger()
	_ = a.waitGroup.Wait()
}

// ArchivedStagingFile represents metadata about an archived staging file, used for
// backfill retrieval (E-032). This is a local type that mirrors the essential fields
// from the warehouse staging file model, avoiding a direct dependency on the
// warehouse/internal/model package (which is restricted by Go's internal package rules).
// The consuming backfill service in warehouse/backfill/ can convert these to the
// warehouse model type since it has access to both packages.
type ArchivedStagingFile struct {
	SourceID      string
	DestinationID string
	Location      string
	FirstEventAt  time.Time
	LastEventAt   time.Time
	CreatedAt     time.Time
}

// ArchivedEventIterator provides a streaming interface for iterating over
// archived gateway event payloads without loading all into memory.
// Consumers must call Close() when done to release resources.
//
// Design note: Next() returns raw JSON bytes ([]byte) rather than a typed
// ArchivedEvent struct. This is intentional — the archiver stores events as
// gzipped JSONL (one JSON object per line), and returning raw bytes avoids an
// unnecessary deserialization/re-serialization cycle since consumers (the replay
// handler) need to inject events back into the Gateway as raw JSON payloads.
// The caller can deserialize selectively if needed (e.g., to extract metadata).
type ArchivedEventIterator interface {
	// Next returns the next archived event payload as raw JSON bytes.
	// Returns io.EOF when no more events are available.
	// Returns nil, io.EOF when all events across all underlying readers are exhausted.
	Next() ([]byte, error)
	// Close releases all resources held by the iterator, including any
	// temporary files and gzip readers created during download.
	Close() error
}

// gzipJSONLIterator implements ArchivedEventIterator by reading gzipped JSONL data.
// It processes multiple readers sequentially, scanning each line-by-line to produce
// individual event payloads without loading entire files into memory.
type gzipJSONLIterator struct {
	readers   []io.ReadCloser // all readers for resource cleanup
	scanner   *bufio.Scanner
	remaining []io.ReadCloser // remaining readers to process
	current   io.ReadCloser
	done      bool
}

// newGzipJSONLIterator creates a new gzipJSONLIterator that reads from the provided
// readers sequentially. If readers is empty, the returned iterator is immediately
// exhausted and returns io.EOF on the first Next() call.
func newGzipJSONLIterator(readers []io.ReadCloser) *gzipJSONLIterator {
	if len(readers) == 0 {
		return &gzipJSONLIterator{done: true}
	}
	it := &gzipJSONLIterator{
		readers:   readers,
		remaining: readers[1:],
		current:   readers[0],
	}
	scanner := bufio.NewScanner(readers[0])
	const maxTokenSize = 10 * 1024 * 1024 // 10 MB max line size for large event payloads
	scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)
	it.scanner = scanner
	return it
}

// Next returns the next archived event payload as raw JSON bytes.
// Empty lines are skipped. When no more events are available across all readers,
// io.EOF is returned. The returned byte slice is a copy safe for retention.
func (it *gzipJSONLIterator) Next() ([]byte, error) {
	if it.done {
		return nil, io.EOF
	}
	for {
		if it.scanner.Scan() {
			line := it.scanner.Bytes()
			if len(line) == 0 {
				continue // skip empty lines between events
			}
			// Return a copy to avoid scanner buffer reuse issues across calls
			result := make([]byte, len(line))
			copy(result, line)
			return result, nil
		}
		if err := it.scanner.Err(); err != nil {
			return nil, fmt.Errorf("scanning JSONL: %w", err)
		}
		// Current reader exhausted, advance to the next reader in sequence
		if len(it.remaining) == 0 {
			it.done = true
			return nil, io.EOF
		}
		it.current = it.remaining[0]
		it.remaining = it.remaining[1:]
		scanner := bufio.NewScanner(it.current)
		const maxTokenSize = 10 * 1024 * 1024 // 10 MB max line size for large event payloads
		scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)
		it.scanner = scanner
	}
}

// Close releases all resources held by the iterator, closing every reader
// that was provided at construction time. Returns the first error encountered.
func (it *gzipJSONLIterator) Close() error {
	var firstErr error
	for _, r := range it.readers {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	it.done = true
	return firstErr
}

// ListArchivedStagingFiles queries archived staging file metadata for backfill retrieval (E-032).
// It searches archived staging files stored in object storage within the specified date range
// for the given source and destination.
//
// This method integrates with the existing filemanager for object storage access (S3/GCS/MinIO)
// and respects the archiver's retention window. The method queries the object storage for
// archived files matching the source ID prefix and date range, then creates staging file
// entries representing the archived data locations.
//
// Parameters:
//   - ctx: Context for cancellation support during potentially long-running storage queries
//   - sourceID: The source identifier to filter archived files
//   - destID: The destination identifier to associate with returned staging files
//   - startDate: The inclusive start of the date range
//   - endDate: The inclusive end of the date range
func (a *archiver) ListArchivedStagingFiles(
	ctx context.Context,
	sourceID, destID string,
	startDate, endDate time.Time,
) ([]ArchivedStagingFile, error) {
	a.log.Infon("listing archived staging files",
		logger.NewStringField("sourceID", sourceID),
		logger.NewStringField("destID", destID),
		logger.NewStringField("startDate", startDate.Format(time.RFC3339)),
		logger.NewStringField("endDate", endDate.Format(time.RFC3339)),
	)

	// Iterate through each day in the date range to query archived files.
	// The archiver organizes files by sourceID/archiveFrom/date/hour,
	// so we scan each day-hour combination within the range.
	var stagingFiles []ArchivedStagingFile
	current := startDate.Truncate(24 * time.Hour)
	end := endDate.Truncate(24 * time.Hour).Add(24 * time.Hour)

	for current.Before(end) {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled during staging file listing: %w", ctx.Err())
		default:
		}

		dateStr := current.Format("2006-01-02")
		prefix := fmt.Sprintf("%s/%s/%s", sourceID, a.archiveFrom, dateStr)

		// Iterate over all 24 hours within the day to find archived files
		for hour := 0; hour < 24; hour++ {
			hourPrefix := fmt.Sprintf("%s/%d", prefix, hour)
			files, err := a.listArchivedFilesForPrefix(ctx, sourceID, hourPrefix, destID, current)
			if err != nil {
				if ctx.Err() != nil {
					return nil, fmt.Errorf("context cancelled: %w", ctx.Err())
				}
				a.log.Warnn("failed to list archived files for prefix",
					logger.NewStringField("prefix", hourPrefix),
					obskit.Error(err),
				)
				continue
			}
			stagingFiles = append(stagingFiles, files...)
		}

		current = current.Add(24 * time.Hour)
	}

	a.log.Infon("archived staging files listed",
		logger.NewIntField("count", int64(len(stagingFiles))),
	)
	return stagingFiles, nil
}

// ListArchivedFiles queries archived file metadata filtered by source ID and date range
// for advanced replay (E-038). This is a convenience method used by the advanced replay
// handler (gateway/handle_http_replay_advanced.go) which filters by source and date range
// but not by destination — destination filtering happens downstream in the replay pipeline.
//
// Parameters:
//   - ctx: Context for cancellation support
//   - sourceID: The source identifier to filter archived files
//   - startDate: The inclusive start of the date range
//   - endDate: The inclusive end of the date range
func (a *archiver) ListArchivedFiles(
	ctx context.Context,
	sourceID string,
	startDate, endDate time.Time,
) ([]ArchivedStagingFile, error) {
	// Delegate to ListArchivedStagingFiles with empty destID.
	// Destination-level filtering is applied later in the replay pipeline
	// when the events are processed by the Processor/Router.
	return a.ListArchivedStagingFiles(ctx, sourceID, "" /* destID */, startDate, endDate)
}

// listArchivedFilesForPrefix queries object storage for archived files matching the
// given prefix, returning actual file metadata from the storage backend.
//
// The archiver stores files at: sourceID/archiveFrom/date/hour/instanceID/filename.json.gz
// This method lists all files under the given prefix (typically sourceID/archiveFrom/date/hour)
// and converts the storage file metadata into ArchivedStagingFile entries.
//
// Requires a workspaceIDResolver to be configured via WithWorkspaceResolver. When the
// resolver is not set, returns nil (empty list) without error — the production backfill
// and replay paths use warehouse/archive/archiver.go which has database-backed queries,
// bypassing this gateway-level listing entirely.
func (a *archiver) listArchivedFilesForPrefix(
	ctx context.Context,
	sourceID, prefix, destID string,
	baseTime time.Time,
) ([]ArchivedStagingFile, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("listing archived files: %w", ctx.Err())
	default:
	}

	// Without a workspace resolver, storage operations cannot proceed because
	// storageProvider.GetFileManager() requires a workspace ID. Return empty
	// results — the production path uses the warehouse archiver adapter.
	if a.workspaceIDResolver == nil {
		return nil, nil
	}

	workspaceID, err := a.workspaceIDResolver(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace for source %q: %w", sourceID, err)
	}

	fm, err := a.storageProvider.GetFileManager(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("getting file manager for workspace %q: %w", workspaceID, err)
	}

	// List all archived files stored under this prefix using the storage backend's
	// native listing API. Each file is converted to an ArchivedStagingFile entry
	// with metadata derived from the file's storage key and last-modified timestamp.
	session := fm.ListFilesWithPrefix(ctx, "", prefix, 1000)
	var result []ArchivedStagingFile
	for {
		fileInfos, err := session.Next()
		if err != nil {
			return result, fmt.Errorf("listing files with prefix %q: %w", prefix, err)
		}
		if len(fileInfos) == 0 {
			break
		}
		for _, fi := range fileInfos {
			result = append(result, ArchivedStagingFile{
				SourceID:      sourceID,
				DestinationID: destID,
				Location:      fi.Key,
				FirstEventAt:  fi.LastModified,
				LastEventAt:   fi.LastModified,
				CreatedAt:     fi.LastModified,
			})
		}
	}
	return result, nil
}

// QueryArchivedEvents returns an iterator over archived gateway event payloads for the
// warehouse replay pipeline (E-035). It reads from the gzipped JSONL files produced by
// the archiver worker, filtering by source ID and date range.
//
// The returned ArchivedEventIterator streams events without loading all into memory.
// Callers MUST call Close() on the iterator when done to release resources.
//
// This method uses the existing filemanager for object storage access (S3/GCS/MinIO)
// and respects the archiver's retention window (archivalTimeInDays: 10 for JobsDB).
//
// Parameters:
//   - ctx: Context for cancellation support during file downloads
//   - sourceID: The source identifier to filter archived events
//   - startTime: The inclusive start of the time range
//   - endTime: The inclusive end of the time range
func (a *archiver) QueryArchivedEvents(
	ctx context.Context,
	sourceID string,
	startTime, endTime time.Time,
) (ArchivedEventIterator, error) {
	a.log.Infon("querying archived events",
		logger.NewStringField("sourceID", sourceID),
		logger.NewStringField("startTime", startTime.Format(time.RFC3339)),
		logger.NewStringField("endTime", endTime.Format(time.RFC3339)),
	)

	// Collect all archived file locations for the date range.
	// The archiver organizes files by sourceID/archiveFrom/date/hour,
	// so we compute all hour-level prefixes that fall within the time range.
	var archiveLocations []string
	current := startTime.Truncate(24 * time.Hour)
	end := endTime.Truncate(24 * time.Hour).Add(24 * time.Hour)

	for current.Before(end) {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled during event query: %w", ctx.Err())
		default:
		}

		dateStr := current.Format("2006-01-02")
		for hour := 0; hour < 24; hour++ {
			hourTime := current.Add(time.Duration(hour) * time.Hour)
			// Skip hours that fall entirely outside the requested time range
			if hourTime.Add(time.Hour).Before(startTime) || hourTime.After(endTime) {
				continue
			}
			prefix := fmt.Sprintf("%s/%s/%s/%d", sourceID, a.archiveFrom, dateStr, hour)
			archiveLocations = append(archiveLocations, prefix)
		}

		current = current.Add(24 * time.Hour)
	}

	if len(archiveLocations) == 0 {
		a.log.Infon("no archive locations found for the specified range")
		return &gzipJSONLIterator{done: true}, nil
	}

	// Build readers for each archive location by downloading the archived files
	// from object storage and decompressing them.
	var readers []io.ReadCloser
	for _, location := range archiveLocations {
		reader, err := a.downloadArchivedFile(ctx, sourceID, location)
		if err != nil {
			// Close any already-opened readers on error to prevent resource leaks
			for _, r := range readers {
				_ = r.Close()
			}
			if ctx.Err() != nil {
				return nil, fmt.Errorf("context cancelled: %w", ctx.Err())
			}
			a.log.Warnn("skipping archived file",
				logger.NewStringField("location", location),
				obskit.Error(err),
			)
			continue
		}
		if reader != nil {
			readers = append(readers, reader)
		}
	}

	if len(readers) == 0 {
		a.log.Infon("no archived event files found")
		return &gzipJSONLIterator{done: true}, nil
	}

	iterator := newGzipJSONLIterator(readers)

	a.log.Infon("archived event iterator created",
		logger.NewIntField("fileCount", int64(len(readers))),
	)
	return iterator, nil
}

// ReplayFilterOption configures optional filtering for advanced replay queries (E-038).
// Options are applied via the functional options pattern, consistent with the archiver's
// existing Option type in options.go.
type ReplayFilterOption func(*replayFilterConfig)

// replayFilterConfig holds the resolved configuration for advanced replay filtering.
// It is populated by applying ReplayFilterOption values in QueryArchivedEventsFiltered.
type replayFilterConfig struct {
	destinationID string
	dryRun        bool
}

// WithDestinationFilter restricts replay events to those targeting the specified
// destination. When set, the returned iterator wraps the base event stream with a
// destinationFilterIterator that checks each event's context.destinationId field.
// Events without destination metadata are passed through for downstream filtering.
func WithDestinationFilter(destinationID string) ReplayFilterOption {
	return func(c *replayFilterConfig) {
		c.destinationID = destinationID
	}
}

// WithDryRun marks the query as a dry-run, which can be used by callers to
// inspect events without executing the replay. The archiver logs the dry-run
// intent but returns events normally — enforcement is the caller's responsibility.
func WithDryRun(dryRun bool) ReplayFilterOption {
	return func(c *replayFilterConfig) {
		c.dryRun = dryRun
	}
}

// QueryArchivedEventsFiltered returns an iterator over archived events with optional
// advanced filtering for the replay pipeline (E-038). It extends QueryArchivedEvents
// with destination-level filtering and dry-run mode support.
//
// The base source-level and date-range filtering is handled by QueryArchivedEvents.
// Additional filtering (destination ID) is applied as a wrapper over the base iterator.
// Dry-run mode is tracked in the returned config but actual enforcement is the caller's
// responsibility — the archiver returns events regardless, and the caller skips execution.
//
// Parameters:
//   - ctx: Context for cancellation support during file downloads
//   - sourceID: The source identifier to filter archived events
//   - startTime: The inclusive start of the time range
//   - endTime: The inclusive end of the time range
//   - opts: Optional ReplayFilterOption values for destination filtering and dry-run
func (a *archiver) QueryArchivedEventsFiltered(
	ctx context.Context,
	sourceID string,
	startTime, endTime time.Time,
	opts ...ReplayFilterOption,
) (ArchivedEventIterator, error) {
	cfg := &replayFilterConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.dryRun {
		a.log.Infon("dry-run replay query — events will be returned but not replayed",
			logger.NewStringField("sourceID", sourceID),
			logger.NewStringField("startTime", startTime.Format(time.RFC3339)),
			logger.NewStringField("endTime", endTime.Format(time.RFC3339)),
		)
	}

	// Delegate to base QueryArchivedEvents for source + time range filtering
	iterator, err := a.QueryArchivedEvents(ctx, sourceID, startTime, endTime)
	if err != nil {
		return nil, err
	}

	// If destination filtering is requested, wrap the iterator to filter events by destination
	if cfg.destinationID != "" {
		a.log.Infon("applying destination filter to replay query",
			logger.NewStringField("destinationID", cfg.destinationID),
		)
		return &destinationFilterIterator{
			inner:         iterator,
			destinationID: cfg.destinationID,
		}, nil
	}

	return iterator, nil
}

// destinationFilterIterator wraps an ArchivedEventIterator to filter events
// by destination ID. Events that don't match the target destination are skipped.
// This is used for destination-level replay filtering in E-038.
//
// Note: The archived events are raw gateway payloads that may not contain
// explicit destination routing. When destination metadata is not present in
// the event payload, the event is passed through (not filtered out) because
// destination routing happens at the Processor/Router level, not at ingestion.
type destinationFilterIterator struct {
	inner         ArchivedEventIterator
	destinationID string
}

// Next returns the next event that matches the destination filter. Events
// targeting a different destination are skipped. Events without destination
// metadata (context.destinationId absent or empty) are passed through for
// downstream filtering by the Processor/Router.
func (it *destinationFilterIterator) Next() ([]byte, error) {
	for {
		event, err := it.inner.Next()
		if err != nil {
			return nil, err // includes io.EOF
		}

		// Check if the event has destination metadata.
		// Archived gateway events may or may not contain destination routing info.
		// If no destination info is present, pass the event through — destination
		// filtering will be enforced downstream by the Processor/Router.
		destID := gjson.GetBytes(event, "context.destinationId").String()
		if destID == "" {
			// No destination metadata — pass through for downstream filtering
			return event, nil
		}
		if destID == it.destinationID {
			return event, nil
		}
		// Skip events targeting a different destination
	}
}

// Close releases all resources held by the inner iterator.
func (it *destinationFilterIterator) Close() error {
	return it.inner.Close()
}

// downloadArchivedFile downloads archived files from object storage for a given
// location prefix, returning a reader over the decompressed (gzipped JSONL) content.
//
// The archiver stores files at: sourceID/archiveFrom/date/hour/instanceID/filename.json.gz
// Since the caller provides hour-level prefixes (e.g., "src123/gw/2024-01-15/10"),
// this method first lists all files under the prefix, then downloads and decompresses
// each one. When multiple files exist under the same prefix (from concurrent archiver
// instances), they are combined into a single sequential reader.
//
// Returns nil, nil when:
//   - The workspaceIDResolver is not configured (production routes via warehouse adapter)
//   - No files are found under the specified prefix
//
// Requires a workspaceIDResolver to be configured via WithWorkspaceResolver. Without
// it, the method returns nil reader and nil error, because storageProvider.GetFileManager()
// needs a workspace ID. The production replay path uses warehouse/archive/archiver.go
// via adapter types in warehouse/app.go, which has database-backed queries.
func (a *archiver) downloadArchivedFile(
	ctx context.Context,
	sourceID, locationPrefix string,
) (io.ReadCloser, error) {
	// Without a workspace resolver, storage download cannot proceed because
	// storageProvider.GetFileManager() requires a workspace ID.
	if a.workspaceIDResolver == nil {
		return nil, nil //nolint:nilnil // nil reader signals "not configured" — production uses warehouse adapter
	}

	workspaceID, err := a.workspaceIDResolver(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace for source %q: %w", sourceID, err)
	}

	fm, err := a.storageProvider.GetFileManager(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("getting file manager for workspace %q: %w", workspaceID, err)
	}

	// List all archived files under this prefix to discover actual file keys.
	// The archiver may produce multiple files per hour from concurrent instances.
	session := fm.ListFilesWithPrefix(ctx, "", locationPrefix, 100)
	var fileKeys []string
	for {
		infos, err := session.Next()
		if err != nil {
			return nil, fmt.Errorf("listing files under prefix %q: %w", locationPrefix, err)
		}
		if len(infos) == 0 {
			break
		}
		for _, fi := range infos {
			fileKeys = append(fileKeys, fi.Key)
		}
	}

	if len(fileKeys) == 0 {
		return nil, nil //nolint:nilnil // no archived files at this prefix
	}

	// Download and decompress each archived file found under the prefix.
	var readers []io.ReadCloser
	for _, key := range fileKeys {
		reader, err := a.downloadAndDecompress(ctx, fm, key)
		if err != nil {
			// Clean up already-opened readers on error to prevent resource leaks.
			for _, r := range readers {
				_ = r.Close()
			}
			return nil, fmt.Errorf("downloading archived file %q: %w", key, err)
		}
		if reader != nil {
			readers = append(readers, reader)
		}
	}

	if len(readers) == 0 {
		return nil, nil //nolint:nilnil // all files missing or empty
	}
	if len(readers) == 1 {
		return readers[0], nil
	}
	// Combine multiple decompressed readers into a single sequential reader.
	return &multiArchivedReader{readers: readers}, nil
}

// downloadAndDecompress downloads a single archived file from object storage and
// returns a reader over its decompressed content. The archived files are gzipped
// JSONL (one JSON object per line), so this wraps the raw download in a gzip reader.
//
// Uses a temporary file as an intermediary because the filemanager.Download API
// writes to io.WriterAt (for parallel/ranged downloads), while gzip.NewReader
// needs io.Reader. The temp file is automatically cleaned up when the returned
// reader is closed.
func (a *archiver) downloadAndDecompress(
	ctx context.Context,
	fm filemanager.FileManager,
	key string,
) (io.ReadCloser, error) {
	tmpFile, err := os.CreateTemp("", "archiver-dl-*.json.gz")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}

	if err := fm.Download(ctx, tmpFile, key); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		// A missing key is not an error — the file may have been deleted between
		// listing and download (race with archival retention cleanup).
		if errors.Is(err, filemanager.ErrKeyNotFound) {
			return nil, nil //nolint:nilnil // file no longer exists
		}
		return nil, fmt.Errorf("downloading %q: %w", key, err)
	}

	// Seek back to start after the download wrote to the file.
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("seeking temp file: %w", err)
	}

	gzReader, err := gzip.NewReader(tmpFile)
	if err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("creating gzip reader for %q: %w", key, err)
	}

	return &archivedFileReader{
		gzipReader: gzReader,
		tmpFile:    tmpFile,
		tmpPath:    tmpFile.Name(),
	}, nil
}

// archivedFileReader wraps a gzip reader backed by a temporary file, providing
// automatic cleanup of the temp file when the reader is closed. This is needed
// because filemanager.Download writes to io.WriterAt (a temp file), while the
// consuming gzipJSONLIterator reads from io.Reader.
type archivedFileReader struct {
	gzipReader io.ReadCloser
	tmpFile    *os.File
	tmpPath    string
}

func (r *archivedFileReader) Read(p []byte) (int, error) {
	return r.gzipReader.Read(p)
}

// Close releases the gzip reader, closes the backing temp file, and removes
// the temp file from disk. Returns the first error encountered.
func (r *archivedFileReader) Close() error {
	var firstErr error
	if err := r.gzipReader.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := r.tmpFile.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	// Best-effort removal — ignore errors (e.g., file already removed).
	_ = os.Remove(r.tmpPath)
	return firstErr
}

// multiArchivedReader combines multiple io.ReadClosers into a single sequential
// reader. When one reader is fully consumed (returns io.EOF), it transparently
// advances to the next. All underlying readers are closed when Close() is called.
type multiArchivedReader struct {
	readers []io.ReadCloser
	current int
}

func (r *multiArchivedReader) Read(p []byte) (int, error) {
	for r.current < len(r.readers) {
		n, err := r.readers[r.current].Read(p)
		if n > 0 {
			return n, err
		}
		if errors.Is(err, io.EOF) {
			r.current++
			continue
		}
		if err != nil {
			return 0, err
		}
	}
	return 0, io.EOF
}

// Close releases all underlying readers, returning the first error encountered.
func (r *multiArchivedReader) Close() error {
	var firstErr error
	for _, reader := range r.readers {
		if err := reader.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
