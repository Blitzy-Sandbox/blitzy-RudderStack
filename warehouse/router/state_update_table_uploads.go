package router

import (
	"fmt"

	"github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
)

func (job *UploadJob) updateTableUploadsCounts() error {
	return job.tableUploadsRepo.WithTx(job.ctx, func(tx *sqlquerywrapper.Tx) error {
		for tableName := range job.upload.UploadSchema {
			// Skip tables excluded by selective sync (E-034).
			// This is defense-in-depth: tables are already filtered at schema generation
			// and table upload creation stages, but we guard here to prevent count
			// propagation for any table that should be excluded.
			if job.selectiveSyncSvc != nil && job.selectiveSyncSvc.IsTableExcluded(
				job.upload.SourceID, job.upload.DestinationID, tableName,
			) {
				continue
			}

			if err := job.tableUploadsRepo.PopulateTotalEventsWithTx(
				job.ctx,
				tx,
				job.upload.ID,
				tableName,
			); err != nil {
				return fmt.Errorf("populate total events from staging file ids for table: %s, %w", tableName, err)
			}
		}
		return nil
	})
}
