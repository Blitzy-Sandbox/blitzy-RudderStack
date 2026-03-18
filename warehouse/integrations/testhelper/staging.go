package testhelper

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/rudderlabs/rudder-server/processor/types"

	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	"github.com/rudderlabs/rudder-go-kit/filemanager"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-server/processor/transformer"
	"github.com/rudderlabs/rudder-server/utils/misc"
	warehouseclient "github.com/rudderlabs/rudder-server/warehouse/client"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

// IdempotentStagingConfig configures the generation of idempotent staging files
// for testing warehouse connector merge/dedup strategies under replay scenarios.
type IdempotentStagingConfig struct {
	TableName       string  // Target table name for the staging events
	EventCount      int     // Number of unique events to generate
	DuplicateRatio  float64 // Ratio of duplicates (0.0 = no dupes, 1.0 = all duped)
	Format          string  // Staging file format (e.g., "json", "csv", "parquet")
	SourceID        string  // Source identifier
	DestinationID   string  // Destination identifier
	WorkspaceID     string  // Workspace identifier
	DestinationType string  // Warehouse destination type (e.g., warehouseutils.SNOWFLAKE)
}

// IdempotentStagingResult contains the output of idempotent staging file generation,
// including paths to the generated files and expected post-dedup checksums for validation.
type IdempotentStagingResult struct {
	StagingFilePaths  []string          // Paths to generated staging files
	ExpectedChecksums map[string]string // Table name → expected SHA256 checksum for validation
	UniqueEventCount  int               // Count of unique events (after dedup)
	TotalEventCount   int               // Total events including duplicates
}

// RenderIdempotentStagingFiles generates staging files with configurable duplicate
// ratios for testing idempotent sync behavior across warehouse connectors. It
// produces deterministic events using seeded random generation and SHA1-based UUIDs,
// enabling reproducible replay/retry scenarios. The returned result includes expected
// post-dedup checksums for validating that warehouse merge strategies produce
// identical state after processing duplicate events.
func RenderIdempotentStagingFiles(t testing.TB, cfg IdempotentStagingConfig) IdempotentStagingResult {
	t.Helper()

	// Fixed namespace UUID for deterministic event ID generation across runs.
	// This ensures that given the same table name and event index, the same
	// UUID is produced regardless of when or where the test executes.
	idempotentNamespace := uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890")

	// Seeded random source for reproducible duplicate selection. The seed is
	// derived from the configuration to ensure identical duplicate patterns
	// across test runs with the same parameters.
	seed := int64(len(cfg.TableName) + cfg.EventCount)
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic seed required for test reproducibility

	// Standard column schema for idempotent testing events.
	columns := map[string]string{
		"id":          "string",
		"received_at": "datetime",
		"event":       "string",
		"user_id":     "string",
	}

	// idempotentEvent represents a single staging event with metadata and data
	// fields compatible with the warehouse staging file format.
	type idempotentEvent struct {
		Metadata map[string]any `json:"metadata"`
		Data     map[string]any `json:"data"`
	}

	// Generate cfg.EventCount unique events, each with a deterministic UUID
	// derived from the namespace and event index via SHA1 hashing.
	uniqueEvents := make([]idempotentEvent, 0, cfg.EventCount)
	for i := 0; i < cfg.EventCount; i++ {
		eventID := uuid.NewSHA1(idempotentNamespace, []byte(fmt.Sprintf("%s-%d", cfg.TableName, i)))
		data := map[string]any{
			"id":          eventID.String(),
			"received_at": "2024-01-15T10:00:00Z",
			"event":       "test_event",
			"user_id":     fmt.Sprintf("user_%d", i),
		}
		ev := idempotentEvent{
			Metadata: map[string]any{
				"table":   cfg.TableName,
				"columns": columns,
			},
			Data: data,
		}
		uniqueEvents = append(uniqueEvents, ev)
	}

	// Inject duplicates: select events using the seeded RNG for reproducibility.
	// The duplicate count is derived from the configured ratio applied to the
	// unique event count.
	duplicateCount := int(float64(cfg.EventCount) * cfg.DuplicateRatio)
	allEvents := make([]idempotentEvent, 0, cfg.EventCount+duplicateCount)
	allEvents = append(allEvents, uniqueEvents...)

	for i := 0; i < duplicateCount; i++ {
		idx := rng.Intn(cfg.EventCount)
		allEvents = append(allEvents, uniqueEvents[idx])
	}

	totalEventCount := cfg.EventCount + duplicateCount

	// Create gzip staging file using the established misc.CreateGZ pattern.
	tmpDir := t.TempDir()
	gzipFilePath := filepath.Join(tmpDir, fmt.Sprintf("idempotent_%s_%d.json.gz", cfg.TableName, time.Now().UnixNano()))

	err := os.MkdirAll(filepath.Dir(gzipFilePath), os.ModePerm)
	require.NoError(t, err)

	gzWriter, err := misc.CreateGZ(gzipFilePath)
	require.NoError(t, err)
	defer func() { _ = gzWriter.CloseGZ() }()

	// Serialize events as newline-delimited JSON using jsonrs (never encoding/json).
	output := new(strings.Builder)
	for _, ev := range allEvents {
		outputJSON, err := jsonrs.Marshal(ev)
		require.NoError(t, err)

		_, err = output.WriteString(string(outputJSON) + "\n")
		require.NoError(t, err)
	}

	err = gzWriter.WriteGZ(output.String())
	require.NoError(t, err)

	// Register cleanup for temporary staging files.
	t.Cleanup(func() {
		if err := os.Remove(gzWriter.File.Name()); err != nil {
			t.Logf("failed to remove idempotent staging temp file: %s", gzWriter.File.Name())
		}
	})

	// Compute SHA256 checksums of unique events only to produce the expected
	// post-dedup state. Only the Data portion of each unique event is hashed,
	// as warehouse merge strategies deduplicate based on event data content.
	hasher := sha256.New()
	for _, ev := range uniqueEvents {
		dataJSON, err := jsonrs.Marshal(ev.Data)
		require.NoError(t, err)

		_, err = hasher.Write(dataJSON)
		require.NoError(t, err)
	}
	hexChecksum := hex.EncodeToString(hasher.Sum(nil))

	return IdempotentStagingResult{
		StagingFilePaths:  []string{gzipFilePath},
		ExpectedChecksums: map[string]string{cfg.TableName: hexChecksum},
		UniqueEventCount:  cfg.EventCount,
		TotalEventCount:   totalEventCount,
	}
}

func createStagingFile(t testing.TB, testConfig *TestConfig) {
	var stagingFile string
	if testConfig.StagingFilePath != "" {
		stagingFile = prepareStagingFilePathUsingStagingFile(t, testConfig)
	} else {
		stagingFile = prepareStagingFilePathUsingEventsFile(t, testConfig)
	}

	uploadOutput := uploadStagingFile(t, testConfig, stagingFile)

	payload := prepareStagingPayload(t, testConfig, stagingFile, uploadOutput)

	url := fmt.Sprintf("http://localhost:%d", testConfig.HTTPPort)
	err := warehouseclient.NewWarehouse(url, stats.NOP).Process(context.Background(), payload)
	require.NoError(t, err)
}

func prepareStagingFilePathUsingStagingFile(t testing.TB, testConfig *TestConfig) string {
	t.Helper()

	path := fmt.Sprintf("%v%v.json", t.TempDir(), fmt.Sprintf("%d.%s.%s", time.Now().Unix(), testConfig.SourceID, uuid.New().String()))
	gzipFilePath := fmt.Sprintf(`%v.gz`, path)

	err := os.MkdirAll(filepath.Dir(gzipFilePath), os.ModePerm)
	require.NoError(t, err)

	gzWriter, err := misc.CreateGZ(gzipFilePath)
	require.NoError(t, err)
	defer func() { _ = gzWriter.CloseGZ() }()

	f, err := os.ReadFile(testConfig.StagingFilePath)
	require.NoError(t, err)

	tpl, err := template.New(uuid.New().String()).Parse(string(f))
	require.NoError(t, err)

	b := new(strings.Builder)

	err = tpl.Execute(b, map[string]any{
		"userID":    testConfig.UserID,
		"sourceID":  testConfig.SourceID,
		"destID":    testConfig.DestinationID,
		"jobRunID":  testConfig.JobRunID,
		"taskRunID": testConfig.TaskRunID,
	})
	require.NoError(t, err)

	err = gzWriter.WriteGZ(b.String())
	require.NoError(t, err)

	t.Cleanup(func() {
		if err := os.Remove(gzWriter.File.Name()); err != nil {
			t.Logf("failed to remove temp file: %s", gzWriter.File.Name())
		}
	})

	return gzipFilePath
}

func prepareStagingFilePathUsingEventsFile(t testing.TB, testConfig *TestConfig) string {
	t.Helper()

	path := fmt.Sprintf("%v%v.json", t.TempDir(), fmt.Sprintf("%d.%s.%s", time.Now().Unix(), testConfig.SourceID, uuid.New().String()))
	gzipFilePath := fmt.Sprintf(`%v.gz`, path)

	err := os.MkdirAll(filepath.Dir(gzipFilePath), os.ModePerm)
	require.NoError(t, err)

	gzWriter, err := misc.CreateGZ(gzipFilePath)
	require.NoError(t, err)
	defer func() { _ = gzWriter.CloseGZ() }()

	f, err := os.ReadFile(testConfig.EventsFilePath)
	require.NoError(t, err)

	tpl, err := template.New(uuid.New().String()).Parse(string(f))
	require.NoError(t, err)

	c := config.New()
	c.Set("DEST_TRANSFORM_URL", testConfig.TransformerURL)
	c.Set("USER_TRANSFORM_URL", testConfig.TransformerURL)

	b := new(strings.Builder)

	destinationJSON, err := jsonrs.Marshal(testConfig.Destination)
	require.NoError(t, err)

	err = tpl.Execute(b, map[string]any{
		"userID":      testConfig.UserID,
		"sourceID":    testConfig.SourceID,
		"workspaceID": testConfig.WorkspaceID,
		"destID":      testConfig.DestinationID,
		"destType":    testConfig.DestinationType,
		"destination": string(destinationJSON),
		"jobRunID":    testConfig.JobRunID,
		"taskRunID":   testConfig.TaskRunID,
	})
	require.NoError(t, err)

	var transformerEvents []types.TransformerEvent
	err = jsonrs.Unmarshal([]byte(b.String()), &transformerEvents)
	require.NoError(t, err)

	tr := transformer.NewClients(c, logger.NOP, stats.Default)
	response := tr.Destination().Transform(context.Background(), transformerEvents)
	require.Zero(t, len(response.FailedEvents))
	responseOutputs := lo.Map(response.Events, func(r types.TransformerResponse, index int) map[string]any {
		return r.Output
	})

	output := new(strings.Builder)
	for _, responseOutput := range responseOutputs {
		outputJSON, err := jsonrs.Marshal(responseOutput)
		require.NoError(t, err)

		_, err = output.WriteString(string(outputJSON) + "\n")
		require.NoError(t, err)
	}

	err = gzWriter.WriteGZ(output.String())
	require.NoError(t, err)

	t.Cleanup(func() {
		if err := os.Remove(gzWriter.File.Name()); err != nil {
			t.Logf("failed to remove temp file: %s", gzWriter.File.Name())
		}
	})

	return gzipFilePath
}

func uploadStagingFile(t testing.TB, testConfig *TestConfig, stagingFile string) filemanager.UploadedFile {
	t.Helper()

	storageProvider := warehouseutils.ObjectStorageType(testConfig.DestinationType, testConfig.Config, false)

	fm, err := filemanager.New(&filemanager.Settings{
		Provider: storageProvider,
		Config: misc.GetObjectStorageConfig(misc.ObjectStorageOptsT{
			Provider:         storageProvider,
			Config:           testConfig.Config,
			UseRudderStorage: misc.IsConfiguredToUseRudderObjectStorage(testConfig.Config),
			WorkspaceID:      testConfig.WorkspaceID,
		}),
		Conf: config.Default,
	})
	require.NoError(t, err)

	keyPrefixes := []string{"rudder-warehouse-staging-logs", testConfig.SourceID, time.Now().Format("2006-01-02")}

	f, err := os.Open(stagingFile)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var uploadOutput filemanager.UploadedFile

	err = WithConstantRetries(func() error {
		if uploadOutput, err = fm.Upload(context.Background(), f, keyPrefixes...); err != nil {
			return fmt.Errorf("uploading staging file: %w", err)
		}

		return nil
	})
	require.NoError(t, err)

	return uploadOutput
}

func prepareStagingPayload(t testing.TB, testConfig *TestConfig, stagingFile string, uploadOutput filemanager.UploadedFile) warehouseclient.StagingFile {
	t.Helper()

	type StagingEvent struct {
		Metadata struct {
			Table   string            `json:"table"`
			Columns map[string]string `json:"columns"`
		}
		Data map[string]any `json:"data"`
	}

	f, err := os.Open(stagingFile)
	require.NoError(t, err)

	reader, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	scanner := bufio.NewScanner(reader)
	schemaMap := make(map[string]map[string]string)

	stagingEvents := make([]StagingEvent, 0)

	for scanner.Scan() {
		lineBytes := scanner.Bytes()

		var stagingEvent StagingEvent
		err := jsonrs.Unmarshal(lineBytes, &stagingEvent)
		require.NoError(t, err)

		stagingEvents = append(stagingEvents, stagingEvent)
	}

	for _, event := range stagingEvents {
		tableName := event.Metadata.Table

		if _, ok := schemaMap[tableName]; !ok {
			schemaMap[tableName] = make(map[string]string)
		}
		for columnName, columnType := range event.Metadata.Columns {
			if _, ok := schemaMap[tableName][columnName]; !ok {
				schemaMap[tableName][columnName] = columnType
			}
		}
	}

	receivedAtProperty := "received_at"
	if testConfig.DestinationType == warehouseutils.SNOWFLAKE {
		receivedAtProperty = "RECEIVED_AT"
	}

	// merge rules and mappings events will not contain received_at, ignoring those
	eventsWithoutIDResolution := lo.Filter(stagingEvents, func(event StagingEvent, index int) bool {
		return event.Metadata.Table != warehouseutils.ToProviderCase(testConfig.DestinationType, warehouseutils.IdentityMergeRulesTable) &&
			event.Metadata.Table != warehouseutils.ToProviderCase(testConfig.DestinationType, warehouseutils.IdentityMappingsTable)
	})

	receivedAt, err := time.Parse(time.RFC3339, eventsWithoutIDResolution[0].Data[receivedAtProperty].(string))
	require.NoError(t, err)

	stagingFileInfo, err := os.Stat(stagingFile)
	require.NoError(t, err)

	bytesPerTable := make(map[string]int64)
	for _, event := range stagingEvents {
		tableName := event.Metadata.Table
		eventJSON, err := jsonrs.Marshal(event.Data)
		require.NoError(t, err)
		bytesPerTable[tableName] += int64(len(eventJSON))
	}

	payload := warehouseclient.StagingFile{
		WorkspaceID:           testConfig.WorkspaceID,
		Schema:                schemaMap,
		SourceID:              testConfig.SourceID,
		DestinationID:         testConfig.DestinationID,
		DestinationRevisionID: testConfig.DestinationID,
		Location:              uploadOutput.ObjectName,
		FirstEventAt:          eventsWithoutIDResolution[0].Data[receivedAtProperty].(string),
		LastEventAt:           eventsWithoutIDResolution[len(eventsWithoutIDResolution)-1].Data[receivedAtProperty].(string),
		TotalEvents:           len(stagingEvents),
		TotalBytes:            int(stagingFileInfo.Size()),
		SourceTaskRunID:       testConfig.TaskRunID,
		SourceJobRunID:        testConfig.JobRunID,
		TimeWindow:            warehouseutils.GetTimeWindow(receivedAt),
		BytesPerTable:         bytesPerTable,
	}
	return payload
}
