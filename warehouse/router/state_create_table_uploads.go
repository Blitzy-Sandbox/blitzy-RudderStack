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
		// Identity tables (merge_rules, mappings) are infrastructure tables that must NEVER
		// be excluded by selective sync. They are required by exportIdentities() which updates
		// table upload records via tableUploadsRepo. Matching the protection pattern in
		// state_export_data.go, identity tables bypass the selective sync check entirely.
		isIdentity := t == whutils.ToProviderCase(destType, whutils.IdentityMergeRulesTable) ||
			t == whutils.ToProviderCase(destType, whutils.IdentityMappingsTable)

		// Skip tables excluded by selective sync (E-034), but never identity tables.
		if !isIdentity && job.selectiveSyncSvc != nil && job.selectiveSyncSvc.IsTableExcluded(
			job.ctx, job.upload.SourceID, job.upload.DestinationID, t,
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
