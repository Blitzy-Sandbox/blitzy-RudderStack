package router

import (
	"fmt"

	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/warehouse/integrations/manager"
)

func (job *UploadJob) createRemoteSchema(whManager manager.Manager) error {
	// Log selective sync status for debugging (E-034)
	if job.selectiveSyncSvc != nil {
		job.logger.Debugn("selective sync service available during schema creation",
			logger.NewStringField("sourceID", job.upload.SourceID),
			logger.NewStringField("destID", job.upload.DestinationID),
		)
	}

	if job.schemaHandle.IsSchemaEmpty(job.ctx) {
		if err := whManager.CreateSchema(job.ctx); err != nil {
			return fmt.Errorf("creating schema: %w", err)
		}
	}
	return nil
}
