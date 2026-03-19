package router

import (
	"fmt"
	"time"

	"github.com/rudderlabs/rudder-go-kit/stats"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/warehouse/healthmonitor"
	"github.com/rudderlabs/rudder-server/warehouse/internal/model"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

const moduleName = "warehouse"

func warehouseTagName(destID, sourceName, destName, sourceID string) string {
	return misc.GetTagName(destID, sourceName, destName, misc.TailTruncateStr(sourceID, 6))
}

func (job *UploadJob) buildTags(extraTags ...warehouseutils.Tag) stats.Tags {
	tags := stats.Tags{
		"module":      moduleName,
		"workspaceId": job.warehouse.WorkspaceID,
		"warehouseID": warehouseTagName(job.warehouse.Destination.ID, job.warehouse.Source.Name, job.warehouse.Destination.Name, job.warehouse.Source.ID),
		"destID":      job.warehouse.Destination.ID,
		"destType":    job.warehouse.Destination.DestinationDefinition.Name,
		"sourceID":    job.warehouse.Source.ID,
		"sourceType":  job.warehouse.Source.SourceDefinition.Name,
	}
	for _, extraTag := range extraTags {
		tags[extraTag.Name] = extraTag.Value
	}
	return tags
}

func (job *UploadJob) timerStat(name string, extraTags ...warehouseutils.Tag) stats.Timer {
	return job.statsFactory.NewTaggedStat(name, stats.TimerType, job.buildTags(extraTags...))
}

func (job *UploadJob) counterStat(name string, extraTags ...warehouseutils.Tag) stats.Counter {
	return job.statsFactory.NewTaggedStat(name, stats.CountType, job.buildTags(extraTags...))
}

func (job *UploadJob) gaugeStat(name string, extraTags ...warehouseutils.Tag) stats.Gauge {
	return job.statsFactory.NewTaggedStat(name, stats.GaugeType, job.buildTags(extraTags...))
}

func (job *UploadJob) histogramStat(name string, extraTags ...warehouseutils.Tag) stats.Histogram {
	return job.statsFactory.NewTaggedStat(name, stats.HistogramType, job.buildTags(extraTags...))
}

func (job *UploadJob) generateUploadSuccessMetrics() {
	var (
		numUploadedEvents int64
		numStagedEvents   int64
		err               error
	)
	numUploadedEvents, err = job.tableUploadsRepo.TotalExportedEvents(
		job.ctx,
		job.upload.ID,
		[]string{},
	)
	if err != nil {
		job.logger.Warnn("sum of total exported events for upload", obskit.Error(err))
		return
	}

	numStagedEvents, err = job.stagingFileRepo.TotalEventsForUploadID(
		job.ctx,
		job.upload.ID,
	)
	if err != nil {
		job.logger.Warnn("total events for upload", obskit.Error(err))
		return
	}

	eventTimeRanges, err := job.stagingFileRepo.GetEventTimeRangesByUploadID(job.ctx, job.upload.ID)
	if err != nil {
		job.logger.Warnn("event time ranges for upload", obskit.Error(err))
		return
	}

	for _, eventTimeRange := range eventTimeRanges {
		job.stats.eventDeliveryTime.Since(eventTimeRange.FirstEventAt)
		job.stats.eventDeliveryTime.Since(eventTimeRange.LastEventAt)
	}

	job.stats.totalRowsSynced.Count(int(numUploadedEvents))
	job.stats.numStagedEvents.Count(int(numStagedEvents))
	job.stats.uploadSuccess.Count(1)

	// Record sync health for E-033 health monitoring on the success path.
	var rowsFailed int64
	if numStagedEvents > numUploadedEvents {
		rowsFailed = numStagedEvents - numUploadedEvents
	}
	job.recordSyncHealth(model.ExportedData, numUploadedEvents, rowsFailed, "")
}

func (job *UploadJob) generateUploadAbortedMetrics() {
	var (
		numUploadedEvents int64
		numStagedEvents   int64
		err               error
	)
	numUploadedEvents, err = job.tableUploadsRepo.TotalExportedEvents(
		job.ctx,
		job.upload.ID,
		[]string{},
	)
	if err != nil {
		job.logger.Warnn("sum of total exported events for upload", obskit.Error(err))
		return
	}

	numStagedEvents, err = job.stagingFileRepo.TotalEventsForUploadID(
		job.ctx,
		job.upload.ID,
	)
	if err != nil {
		job.logger.Warnn("total events for upload", obskit.Error(err))
		return
	}

	job.stats.totalRowsSynced.Count(int(numUploadedEvents))
	job.stats.numStagedEvents.Count(int(numStagedEvents))

	// Record sync health for E-033 health monitoring on the abort/failure path.
	var rowsFailed int64
	if numStagedEvents > numUploadedEvents {
		rowsFailed = numStagedEvents - numUploadedEvents
	}
	job.recordSyncHealth(model.Aborted, numUploadedEvents, rowsFailed, model.UncategorizedError)
}

func (job *UploadJob) recordTableLoad(tableName string, numEvents int64) {
	capturedTableName := warehouseutils.TableNameForStats(tableName)

	job.counterStat(`event_delivery`, warehouseutils.Tag{
		Name:  "tableName",
		Value: capturedTableName,
	}).Count(int(numEvents))

	job.counterStat(`rows_synced`, warehouseutils.Tag{
		Name:  "tableName",
		Value: capturedTableName,
	}).Count(int(numEvents))
}

func (job *UploadJob) recordLoadFileGenerationTimeStat(startID, endID int64) error {
	startLoadFile, err := job.loadFilesRepo.GetByID(job.ctx, startID)
	if err != nil {
		return fmt.Errorf("getting start load file by id %d: %w", startID, err)
	}
	endLoadFile, err := job.loadFilesRepo.GetByID(job.ctx, endID)
	if err != nil {
		return fmt.Errorf("getting end load file by id %d: %w", endID, err)
	}

	job.stats.loadFileGenerationTime.SendTiming(endLoadFile.CreatedAt.Sub(startLoadFile.CreatedAt))
	return nil
}

// recordSyncHealth constructs a SyncHealth record from the upload job state and
// persists it via the health monitor (E-033). This feeds the periodic health
// collection loop, Prometheus metric emission, and alerting evaluation pipeline.
//
// The method is a no-op when the health monitor is not configured (nil), which
// happens when health monitoring is disabled or when running tests that do not
// wire the health monitor. Errors from the health monitor are logged at warn
// level but do not interrupt the upload pipeline — health recording is best-effort.
func (job *UploadJob) recordSyncHealth(status string, rowsSynced, rowsFailed int64, errorCategory string) {
	if job.healthMonitor == nil {
		return
	}

	// Compute sync duration from upload timings if available.
	// Timings is a slice of maps: [{status1: time1}, {status2: time2}, ...].
	// We find the earliest and latest timestamps across all timing entries.
	var durationMs int64
	if len(job.upload.Timings) > 0 {
		var earliest, latest time.Time
		for _, entry := range job.upload.Timings {
			for _, ts := range entry {
				if earliest.IsZero() || ts.Before(earliest) {
					earliest = ts
				}
				if latest.IsZero() || ts.After(latest) {
					latest = ts
				}
			}
		}
		if !earliest.IsZero() && !latest.IsZero() {
			durationMs = latest.Sub(earliest).Milliseconds()
		}
	}

	health := &healthmonitor.SyncHealth{
		UploadID:      job.upload.ID,
		SourceID:      job.warehouse.Source.ID,
		DestinationID: job.warehouse.Destination.ID,
		DestType:      job.warehouse.Destination.DestinationDefinition.Name,
		SourceType:    job.warehouse.Source.SourceDefinition.Name,
		WorkspaceID:   job.warehouse.WorkspaceID,
		SourceName:    job.warehouse.Source.Name,
		DestName:      job.warehouse.Destination.Name,
		Status:        status,
		DurationMs:    durationMs,
		RowsSynced:    rowsSynced,
		RowsFailed:    rowsFailed,
		ErrorCategory: errorCategory,
	}

	if err := job.healthMonitor.RecordSyncHealth(job.ctx, health); err != nil {
		job.logger.Warnn("failed to record sync health", obskit.Error(err))
	}
}
