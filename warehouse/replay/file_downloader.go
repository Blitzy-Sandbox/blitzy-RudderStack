// Package replay provides the warehouse replay feature (E-035) for re-processing
// archived events through the warehouse pipeline.
//
// This file implements StagingFileDownloader, the concrete FileDownloader implementation
// that downloads archived staging files from cloud storage using the filemanager library.
// The archiver's QueryArchivedEvents returns batches with a Location field (cloud storage
// path) and empty Data — the StagingFileDownloader resolves the actual file content.
//
// The storage provider and configuration are determined by environment-based config
// (JOBS_BACKUP_STORAGE_PROVIDER for backups, or the Rudder default S3 config for
// staging files), matching the pattern used by warehouse/archive/archiver.go.
package replay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/filemanager"
	"github.com/rudderlabs/rudder-go-kit/logger"

	"github.com/rudderlabs/rudder-server/utils/filemanagerutil"
)

// StagingFileDownloader implements the FileDownloader interface by downloading
// archived staging files from cloud storage using the filemanager library.
//
// It creates a file manager configured with the JOBS_BACKUP_STORAGE_PROVIDER
// (default "S3") and the corresponding provider config from environment variables,
// matching the storage configuration pattern used by the warehouse archiver
// (warehouse/archive/archiver.go lines 157-160).
//
// This implementation is designed for the warehouse replay pipeline, where
// archived staging files must be downloaded from the same cloud storage location
// where the archiver originally stored them.
type StagingFileDownloader struct {
	conf               *config.Config
	log                logger.Logger
	fileManagerFactory filemanager.Factory
}

// NewStagingFileDownloader creates a new StagingFileDownloader with the provided
// dependencies.
//
// Parameters:
//   - conf: Configuration instance for resolving storage provider settings
//   - log: Structured logger for diagnostic output
//   - fileManagerFactory: Factory for creating storage-provider-specific file managers
func NewStagingFileDownloader(
	conf *config.Config,
	log logger.Logger,
	fileManagerFactory filemanager.Factory,
) *StagingFileDownloader {
	return &StagingFileDownloader{
		conf:               conf,
		log:                log.Child("replay.downloader"),
		fileManagerFactory: fileManagerFactory,
	}
}

// Download retrieves the file content from the specified cloud storage location
// and returns the raw bytes. The location is the cloud storage path as stored in
// the wh_staging_files table's location column.
//
// The method creates a temporary file for the download (required by the filemanager
// API which writes to io.WriterAt), reads its content into memory, and cleans up
// the temporary file. This follows the same pattern used by warehouse/api/grpc.go
// (lines 820-840) for downloading warehouse validation files.
//
// The storage provider is resolved from the JOBS_BACKUP_STORAGE_PROVIDER environment
// variable (default "S3"), with provider-specific configuration resolved from
// environment using filemanagerutil.GetProviderConfigForBackupsFromEnv.
func (d *StagingFileDownloader) Download(ctx context.Context, location string) ([]byte, error) {
	provider := d.conf.GetString("JOBS_BACKUP_STORAGE_PROVIDER", "S3")
	fm, err := d.fileManagerFactory(&filemanager.Settings{
		Provider: provider,
		Config:   filemanagerutil.GetProviderConfigForBackupsFromEnv(ctx, d.conf),
		Conf:     d.conf,
	})
	if err != nil {
		return nil, fmt.Errorf("creating file manager for provider %q: %w", provider, err)
	}

	// Create a temporary file for the download — the filemanager.Download API
	// requires an io.WriterAt (typically *os.File).
	tmpFile, err := os.CreateTemp("", "replay-download-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp file for download: %w", err)
	}
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}()

	downloadKey := fm.GetDownloadKeyFromFileLocation(location)
	if err := fm.Download(ctx, tmpFile, downloadKey); err != nil {
		return nil, fmt.Errorf("downloading file from %q (key: %q): %w", location, downloadKey, err)
	}

	// Seek back to beginning and read all content into memory.
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking temp file: %w", err)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, tmpFile); err != nil {
		return nil, fmt.Errorf("reading downloaded file: %w", err)
	}

	d.log.Debugn("downloaded archived staging file",
		logger.NewStringField("location", location),
		logger.NewIntField("bytes", int64(buf.Len())),
	)

	return buf.Bytes(), nil
}
