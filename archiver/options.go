package archiver

import (
	"context"
	"time"

	"github.com/rudderlabs/rudder-server/utils/payload"
)

type Option func(*archiver)

func WithAdaptiveLimit(limiter payload.AdaptiveLimiterFunc) Option {
	return func(a *archiver) {
		a.adaptivePayloadLimitFunc = limiter
	}
}

func WithArchiveTrigger(trigger func() <-chan time.Time) Option {
	return func(a *archiver) {
		a.archiveTrigger = trigger
	}
}

func WithArchiveFrom(archiveFrom string) Option {
	return func(a *archiver) {
		a.archiveFrom = archiveFrom
	}
}

// WithWorkspaceResolver injects a function that resolves a source ID to its
// workspace ID, enabling the archiver's storage-backed operations (file listing,
// file download) for the backfill and replay methods. The storageProvider's
// GetFileManager requires a workspace ID to resolve the correct storage backend.
//
// When not set, ListArchivedStagingFiles and QueryArchivedEvents return empty
// results without error — the production backfill and replay paths use the
// warehouse archiver (warehouse/archive/archiver.go) via adapter types in
// warehouse/app.go, which has database-backed queries and bypasses these
// gateway-level storage operations entirely.
//
// Usage:
//
//	archiver.New(jobsDB, storageProvider, conf, stats,
//	    archiver.WithWorkspaceResolver(func(ctx context.Context, sourceID string) (string, error) {
//	        return backendConfig.GetWorkspaceIDForSource(ctx, sourceID)
//	    }),
//	)
func WithWorkspaceResolver(resolver func(ctx context.Context, sourceID string) (string, error)) Option {
	return func(a *archiver) {
		a.workspaceIDResolver = resolver
	}
}
