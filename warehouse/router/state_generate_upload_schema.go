package router

import (
	"fmt"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/warehouse/internal/model"
	"github.com/rudderlabs/rudder-server/warehouse/internal/repo"
)

func (job *UploadJob) generateUploadSchema() error {
	uploadSchema, err := job.schemaHandle.ConsolidateStagingFilesSchema(job.ctx, job.stagingFiles, nil)
	if err != nil {
		return fmt.Errorf("consolidate staging files schema using warehouse schema: %w", err)
	}

	// Apply selective sync schema filtering (E-034).
	// Remove excluded tables and columns from the consolidated schema
	// before persisting it to the upload record.
	if job.selectiveSyncSvc != nil {
		uploadSchema = job.filterSchemaBySelectiveSync(uploadSchema)
	}

	uploadSchemaBytes, err := jsonrs.Marshal(uploadSchema)
	if err != nil {
		return fmt.Errorf("marshal upload schema: %w", err)
	}
	job.stats.consolidatedSchemaSize.Observe(float64(len(uploadSchemaBytes)))

	err = job.uploadsRepo.Update(
		job.ctx,
		job.upload.ID,
		[]repo.UpdateKeyValue{
			repo.UploadFieldSchema(uploadSchemaBytes),
		},
	)
	if err != nil {
		return fmt.Errorf("set upload schema: %w", err)
	}

	job.upload.UploadSchema = uploadSchema

	return nil
}

// filterSchemaBySelectiveSync removes excluded tables and columns from the schema
// based on the selective sync configuration for this upload's source/destination pair.
// Tables that are entirely excluded or whose columns are all excluded are removed
// from the returned schema. This ensures downstream pipeline stages (load file generation,
// table upload creation, data export) only process included tables and columns.
func (job *UploadJob) filterSchemaBySelectiveSync(schema model.Schema) model.Schema {
	if job.selectiveSyncSvc == nil {
		return schema
	}

	sourceID := job.upload.SourceID
	destID := job.upload.DestinationID
	filteredSchema := make(model.Schema)

	for tableName, tableSchema := range schema {
		// Skip excluded tables
		if job.selectiveSyncSvc.IsTableExcluded(job.ctx, sourceID, destID, tableName) {
			job.logger.Debugn("selective sync: excluding table from upload schema",
				logger.NewStringField("table", tableName),
			)
			continue
		}

		// Filter excluded columns within included tables
		filteredTableSchema := make(model.TableSchema)
		for columnName, columnType := range tableSchema {
			if !job.selectiveSyncSvc.IsColumnExcluded(job.ctx, sourceID, destID, tableName, columnName) {
				filteredTableSchema[columnName] = columnType
			}
		}

		// Only include tables that still have at least one column after filtering
		if len(filteredTableSchema) > 0 {
			filteredSchema[tableName] = filteredTableSchema
		}
	}

	return filteredSchema
}
