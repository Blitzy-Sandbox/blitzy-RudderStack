package router

import (
	"slices"

	"github.com/rudderlabs/rudder-go-kit/logger"

	whutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

func (job *UploadJob) createTableUploads() error {
	schemaForUpload := job.upload.UploadSchema
	destType := job.warehouse.Type
	tables := make([]string, 0, len(schemaForUpload))
	for t := range schemaForUpload {
		// Skip tables excluded by selective sync (E-034).
		// Identity tables are infrastructure tables and bypass selective sync
		// exclusion — they should never appear in the exclusion configuration.
		if job.selectiveSyncSvc != nil && job.selectiveSyncSvc.IsTableExcluded(
			job.upload.SourceID, job.upload.DestinationID, t,
		) {
			job.logger.Debugn("selective sync: skipping table upload creation for excluded table",
				logger.NewStringField("table", t),
			)
			continue
		}
		tables = append(tables, t)
		// also track upload to rudder_identity_mappings if the upload has records for rudder_identity_merge_rules
		if slices.Contains(whutils.IdentityEnabledWarehouses, destType) && t == whutils.ToProviderCase(destType, whutils.IdentityMergeRulesTable) {
			if _, ok := schemaForUpload[whutils.ToProviderCase(destType, whutils.IdentityMappingsTable)]; !ok {
				tables = append(tables, whutils.ToProviderCase(destType, whutils.IdentityMappingsTable))
			}
		}
	}
	return job.tableUploadsRepo.Insert(
		job.ctx,
		job.upload.ID,
		tables,
	)
}
