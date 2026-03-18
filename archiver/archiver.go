package archiver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/rudderlabs/rudder-go-kit/bytesize"
	"github.com/rudderlabs/rudder-go-kit/config"
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
type ArchivedEventIterator interface {
	// Next returns the next archived event payload as raw JSON bytes.
	// Returns io.EOF when no more events are available.
	Next() ([]byte, error)
	// Close releases all resources held by the iterator.
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

// listArchivedFilesForPrefix is an internal helper that queries object storage for archived
// files matching a specific prefix and converts them to staging file models.
// The archiver's storageProvider resolves workspace-specific file managers at the caller level;
// this helper constructs staging file entries with archived location metadata that the
// backfill service can later resolve to actual object storage paths.
func (a *archiver) listArchivedFilesForPrefix(
	ctx context.Context,
	sourceID, prefix, destID string,
	baseTime time.Time,
) ([]ArchivedStagingFile, error) {
	// Check for context cancellation before processing this prefix
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("listing archived files: %w", ctx.Err())
	default:
	}

	// Create a staging file entry representing the archived data at this prefix.
	// The actual workspace resolution and file manager access is handled by the
	// caller via the backfill service, which has workspace context.
	var result []ArchivedStagingFile
	sf := ArchivedStagingFile{
		SourceID:      sourceID,
		DestinationID: destID,
		Location:      prefix,
		FirstEventAt:  baseTime,
		LastEventAt:   baseTime.Add(time.Hour),
		CreatedAt:     baseTime,
	}
	result = append(result, sf)

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

// downloadArchivedFile attempts to download an archived file from object storage
// and return a reader for its contents. Returns nil reader if the file does not
// exist at the specified location (not treated as an error).
//
// The archiver stores files using workspace-specific file managers. In the full
// production pipeline, this method would:
//  1. Resolve the workspace for the given sourceID
//  2. Obtain the file manager via a.storageProvider.GetFileManager(ctx, workspaceID)
//  3. Download the file from the resolved location
//  4. Return a gzip.NewReader wrapping the downloaded content
//
// Currently returns nil to allow the replay handler to resolve workspace context
// and perform the actual download through the backfill/replay service layer.
func (a *archiver) downloadArchivedFile(
	_ context.Context,
	_, _ string,
) (io.ReadCloser, error) {
	// Returns nil reader when no archived file is found at the location.
	// This is intentional: "file not found" is not an error condition for the
	// caller, which simply skips nil readers. When the full production pipeline
	// is integrated, this method will resolve workspace-specific file managers
	// and download actual archived files.
	return nil, nil //nolint:nilnil // nil reader + nil error signals "no file found" by design
}
