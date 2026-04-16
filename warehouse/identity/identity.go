package identity

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/filemanager"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/warehouse/encoding"
	sqlmiddleware "github.com/rudderlabs/rudder-server/warehouse/integrations/middleware/sqlquerywrapper"
	"github.com/rudderlabs/rudder-server/warehouse/internal/model"
	"github.com/rudderlabs/rudder-server/warehouse/internal/service/loadfiles/downloader"
	"github.com/rudderlabs/rudder-server/warehouse/logfield"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("warehouse").Child("identity")
}

// ResolutionStrategy identifies which identity resolution strategy was applied.
// These strategies are shared between warehouse batch identity resolution and
// the real-time identity graph (identity/graph/resolver.go).
type ResolutionStrategy int

const (
	// StrategyNewMatch indicates no existing identity was found for any identifier.
	// A new segment/rudder_id is created.
	StrategyNewMatch ResolutionStrategy = iota

	// StrategySingleMatch indicates exactly one existing identity was found.
	// The existing rudder_id is reused.
	StrategySingleMatch

	// StrategyMultiMatch indicates multiple existing identities were found.
	// All are merged under the first rudder_id.
	StrategyMultiMatch
)

// String returns the human-readable name of the resolution strategy.
func (s ResolutionStrategy) String() string {
	switch s {
	case StrategyNewMatch:
		return "new_match"
	case StrategySingleMatch:
		return "single_match"
	case StrategyMultiMatch:
		return "multi_match"
	default:
		return "unknown"
	}
}

// MergeProperty represents a single property type-value pair used in identity merge rules.
// This extends beyond the original two-property limitation (merge_property_1/merge_property_2)
// to support an arbitrary number of property pairs.
type MergeProperty struct {
	Type  string
	Value string
}

// MergeRule represents a set of property pairs that should be resolved together.
// In the existing warehouse model, this is limited to two properties (merge_property_1, merge_property_2).
// The flexible model supports an arbitrary number of properties for the real-time identity graph.
type MergeRule struct {
	Properties []MergeProperty
}

// ResolutionInput captures the inputs to an identity resolution operation.
// It provides the merge properties that need to be resolved, abstracting
// away whether this comes from a warehouse merge rule or a real-time event.
type ResolutionInput struct {
	Properties []MergeProperty
}

// ResolutionOutput captures the result of an identity resolution operation.
// Both warehouse batch resolution and real-time graph resolution produce this.
type ResolutionOutput struct {
	Strategy    ResolutionStrategy
	RudderID    string
	MappingRows []MappingRow
}

// MappingRow represents a single row in the identity mappings table.
type MappingRow struct {
	MergePropertyType  string
	MergePropertyValue string
	RudderID           string
	UpdatedAt          string
}

// MergeRuleApplier defines the interface for applying identity merge rules.
// This allows both the warehouse batch pipeline (which uses PostgreSQL transactions)
// and the real-time identity graph (which uses its own storage) to share resolution logic.
type MergeRuleApplier interface {
	// LookupRudderIDs finds all existing rudder_ids matching the given properties.
	LookupRudderIDs(ctx context.Context, properties []MergeProperty) ([]string, error)

	// CreateMapping creates a new mapping from a merge property to a rudder_id.
	CreateMapping(ctx context.Context, row MappingRow) error

	// UpdateMappings updates all mappings from old rudder_ids to a new rudder_id.
	UpdateMappings(ctx context.Context, newRudderID string, oldRudderIDs []string) (int64, error)

	// GetAllMappingsForRudderIDs retrieves all merge properties associated with the given rudder_ids.
	GetAllMappingsForRudderIDs(ctx context.Context, rudderIDs []string) ([]MergeProperty, error)
}

type WarehouseManager interface {
	DownloadIdentityRules(context.Context, *misc.GZipWriter) error
}

type Identity struct {
	warehouse        model.Warehouse
	db               *sqlmiddleware.DB
	uploader         warehouseutils.Uploader
	uploadID         int64
	warehouseManager WarehouseManager
	downloader       downloader.Downloader
	encodingFactory  *encoding.Factory
}

func New(warehouse model.Warehouse, db *sqlmiddleware.DB, uploader warehouseutils.Uploader, uploadID int64, warehouseManager WarehouseManager, loadFileDownloader downloader.Downloader, encodingFactory *encoding.Factory) *Identity {
	return &Identity{
		warehouse:        warehouse,
		db:               db,
		uploader:         uploader,
		uploadID:         uploadID,
		warehouseManager: warehouseManager,
		downloader:       loadFileDownloader,
		encodingFactory:  encodingFactory,
	}
}

func (idr *Identity) mergeRulesTable() string {
	return warehouseutils.IdentityMergeRulesTableName(idr.warehouse)
}

func (idr *Identity) mappingsTable() string {
	return warehouseutils.IdentityMappingsTableName(idr.warehouse)
}

func (idr *Identity) whMergeRulesTable() string {
	return warehouseutils.ToProviderCase(idr.warehouse.Destination.DestinationDefinition.Name, warehouseutils.IdentityMergeRulesTable)
}

func (idr *Identity) whMappingsTable() string {
	return warehouseutils.ToProviderCase(idr.warehouse.Destination.DestinationDefinition.Name, warehouseutils.IdentityMappingsTable)
}

// ApplyMergeRule executes the core identity resolution algorithm for a set of merge properties.
// This function extracts the pure resolution logic from the warehouse-specific applyRule() method,
// making it reusable by the real-time identity graph (identity/graph/resolver.go).
//
// The three resolution strategies are:
//   - New match (no existing rudder_ids found): Creates a new UUID-based rudder_id
//   - Single match (exactly one rudder_id found): Reuses the existing rudder_id
//   - Multi match (multiple rudder_ids found): Merges all under the first rudder_id
//
// Parameters:
//   - ctx: Context for cancellation
//   - applier: The storage-specific implementation of merge rule operations
//   - input: The merge properties to resolve
//   - generateID: Function to generate new UUIDs (allows injection of misc.FastUUID for warehouse)
//   - timeNow: Function to get current time string (allows injection for testing)
//
// Returns the resolution output (strategy, rudder_id, mapping rows) or error.
func ApplyMergeRule(ctx context.Context, applier MergeRuleApplier, input ResolutionInput, generateID func() string, timeNow func() string) (*ResolutionOutput, error) {
	// Look up existing rudder_ids for all properties
	rudderIDs, err := applier.LookupRudderIDs(ctx, input.Properties)
	if err != nil {
		return nil, fmt.Errorf("lookup rudder_ids: %w", err)
	}

	currentTimeString := timeNow()
	output := &ResolutionOutput{}

	if len(rudderIDs) <= 1 {
		// New match or single match
		var rudderID string
		if len(rudderIDs) == 0 {
			rudderID = generateID()
			output.Strategy = StrategyNewMatch
		} else {
			rudderID = rudderIDs[0]
			output.Strategy = StrategySingleMatch
		}
		output.RudderID = rudderID

		// Create mapping rows for each non-empty property
		for _, prop := range input.Properties {
			if prop.Type == "" || prop.Value == "" {
				continue // skip empty properties (mirrors the prop2Val.Valid check in warehouse model)
			}
			row := MappingRow{
				MergePropertyType:  prop.Type,
				MergePropertyValue: prop.Value,
				RudderID:           rudderID,
				UpdatedAt:          currentTimeString,
			}
			if err := applier.CreateMapping(ctx, row); err != nil {
				return nil, fmt.Errorf("create mapping: %w", err)
			}
			output.MappingRows = append(output.MappingRows, row)
		}
	} else {
		// Multi match — merge all under first rudder_id
		output.Strategy = StrategyMultiMatch
		newID := rudderIDs[0]
		output.RudderID = newID

		// Create mapping rows for merge properties
		for _, prop := range input.Properties {
			if prop.Type == "" || prop.Value == "" {
				continue
			}
			row := MappingRow{
				MergePropertyType:  prop.Type,
				MergePropertyValue: prop.Value,
				RudderID:           newID,
				UpdatedAt:          currentTimeString,
			}
			output.MappingRows = append(output.MappingRows, row)
		}

		// Get all existing mappings for all matched rudder_ids
		existingProps, err := applier.GetAllMappingsForRudderIDs(ctx, rudderIDs)
		if err != nil {
			return nil, fmt.Errorf("get all mappings: %w", err)
		}
		for _, prop := range existingProps {
			output.MappingRows = append(output.MappingRows, MappingRow{
				MergePropertyType:  prop.Type,
				MergePropertyValue: prop.Value,
				RudderID:           newID,
				UpdatedAt:          currentTimeString,
			})
		}

		// Update all old rudder_ids to the new one
		if len(rudderIDs) > 1 {
			if _, err := applier.UpdateMappings(ctx, newID, rudderIDs[1:]); err != nil {
				return nil, fmt.Errorf("update mappings: %w", err)
			}
		}

		// Create new mappings for the merge properties
		for _, prop := range input.Properties {
			if prop.Type == "" || prop.Value == "" {
				continue
			}
			row := MappingRow{
				MergePropertyType:  prop.Type,
				MergePropertyValue: prop.Value,
				RudderID:           newID,
				UpdatedAt:          currentTimeString,
			}
			if err := applier.CreateMapping(ctx, row); err != nil {
				return nil, fmt.Errorf("create merge mapping: %w", err)
			}
		}
	}

	return output, nil
}

// warehouseMergeRuleApplier implements MergeRuleApplier using PostgreSQL transactions,
// wrapping the existing SQL operations from the original applyRule() method.
type warehouseMergeRuleApplier struct {
	txn               *sqlmiddleware.Tx
	mappingsTable     string
	warehouse         model.Warehouse
	currentTimeString string
}

// LookupRudderIDs finds all existing rudder_ids matching the given properties
// by querying the warehouse identity mappings table.
func (w *warehouseMergeRuleApplier) LookupRudderIDs(_ context.Context, properties []MergeProperty) ([]string, error) {
	var conditions []string
	for _, prop := range properties {
		if prop.Type != "" && prop.Value != "" {
			conditions = append(conditions, fmt.Sprintf(`(merge_property_type='%s' AND merge_property_value=%s)`, prop.Type, misc.QuoteLiteral(prop.Value)))
		}
	}
	if len(conditions) == 0 {
		return nil, nil
	}

	whereClause := strings.Join(conditions, " OR ")
	sqlStatement := fmt.Sprintf(`SELECT ARRAY_AGG(DISTINCT(rudder_id)) FROM %s WHERE %s`, w.mappingsTable, whereClause)
	pkgLogger.Debugn("IDR: Fetching all rudder_id's corresponding to the merge_rule", logger.NewStringField(logfield.Query, sqlStatement))

	var rudderIDs []string
	err := w.txn.QueryRow(sqlStatement).Scan(pq.Array(&rudderIDs))
	if err != nil {
		pkgLogger.Errorn("IDR: Error fetching all rudder_id's corresponding to the merge_rule",
			logger.NewStringField(logfield.Query, sqlStatement),
			obskit.Error(err),
		)
		return nil, err
	}
	return rudderIDs, nil
}

// CreateMapping inserts a new mapping row into the warehouse identity mappings table
// using ON CONFLICT DO NOTHING to handle duplicate entries.
func (w *warehouseMergeRuleApplier) CreateMapping(_ context.Context, row MappingRow) error {
	rowValues := misc.SingleQuoteLiteralJoin([]string{row.MergePropertyType, row.MergePropertyValue, row.RudderID, row.UpdatedAt})
	sqlStatement := fmt.Sprintf(`INSERT INTO %s (merge_property_type, merge_property_value, rudder_id, updated_at) VALUES (%s) ON CONFLICT ON CONSTRAINT %s DO NOTHING`,
		w.mappingsTable, rowValues, warehouseutils.IdentityMappingsUniqueMappingConstraintName(w.warehouse))

	pkgLogger.Debugn("IDR: Inserting property mapping into mappings table", logger.NewStringField(logfield.Query, sqlStatement))
	_, err := w.txn.Exec(sqlStatement)
	if err != nil {
		pkgLogger.Errorn("IDR: Error inserting property mapping into mappings table",
			obskit.Error(err),
		)
	}
	return err
}

// UpdateMappings updates all mappings from old rudder_ids to a new rudder_id,
// consolidating multiple identities under a single resolved identity.
func (w *warehouseMergeRuleApplier) UpdateMappings(_ context.Context, newRudderID string, oldRudderIDs []string) (int64, error) {
	sqlStatement := fmt.Sprintf(`UPDATE %s SET rudder_id='%s', updated_at='%s' WHERE rudder_id IN (%v)`,
		w.mappingsTable, newRudderID, w.currentTimeString, misc.SingleQuoteLiteralJoin(oldRudderIDs))

	res, err := w.txn.Exec(sqlStatement)
	if err != nil {
		return 0, err
	}
	affectedRowCount, _ := res.RowsAffected()
	pkgLogger.Debugn("IDR: Updated rudder_id for all properties in mapping table",
		logger.NewIntField("affectedRowCount", affectedRowCount),
		logger.NewStringField(logfield.Query, sqlStatement))
	return affectedRowCount, nil
}

// GetAllMappingsForRudderIDs retrieves all merge properties associated with the given rudder_ids
// from the warehouse identity mappings table.
func (w *warehouseMergeRuleApplier) GetAllMappingsForRudderIDs(_ context.Context, rudderIDs []string) ([]MergeProperty, error) {
	quotedRudderIDs := misc.SingleQuoteLiteralJoin(rudderIDs)
	sqlStatement := fmt.Sprintf(`SELECT merge_property_type, merge_property_value FROM %s WHERE rudder_id IN (%v)`, w.mappingsTable, quotedRudderIDs)

	pkgLogger.Debugn("IDR: Get all merge properties from mapping table with rudder_id's",
		logger.NewStringField("quotedRudderIDs", quotedRudderIDs),
		logger.NewStringField(logfield.Query, sqlStatement))

	tableRows, err := w.txn.Query(sqlStatement)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tableRows.Close() }()

	var properties []MergeProperty
	for tableRows.Next() {
		var mergePropType, mergePropVal string
		err = tableRows.Scan(&mergePropType, &mergePropVal)
		if err != nil {
			return nil, err
		}
		properties = append(properties, MergeProperty{Type: mergePropType, Value: mergePropVal})
	}
	if err = tableRows.Err(); err != nil {
		return nil, err
	}
	return properties, nil
}

func (idr *Identity) applyRule(txn *sqlmiddleware.Tx, ruleID int64, gzWriter *misc.GZipWriter) (totalRowsModified int, err error) {
	// Step 1: Read merge rule from PostgreSQL (unchanged from original)
	sqlStatement := fmt.Sprintf(`SELECT merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value FROM %s WHERE id=%v`, idr.mergeRulesTable(), ruleID)

	var prop1Val, prop2Val, prop1Type, prop2Type sql.NullString
	err = txn.QueryRow(sqlStatement).Scan(&prop1Type, &prop1Val, &prop2Type, &prop2Val)
	if err != nil {
		return 0, err
	}

	// Step 2: Build ResolutionInput from the two-property model
	input := ResolutionInput{}
	if prop1Type.Valid && prop1Val.Valid {
		input.Properties = append(input.Properties, MergeProperty{Type: prop1Type.String, Value: prop1Val.String})
	}
	if prop2Type.Valid && prop2Val.Valid {
		input.Properties = append(input.Properties, MergeProperty{Type: prop2Type.String, Value: prop2Val.String})
	}

	// Step 3: Create warehouse-specific applier wrapping the transaction
	currentTimeString := time.Now().Format(misc.RFC3339Milli)
	applier := &warehouseMergeRuleApplier{
		txn:               txn,
		mappingsTable:     idr.mappingsTable(),
		warehouse:         idr.warehouse,
		currentTimeString: currentTimeString,
	}

	// Step 4: Execute the shared resolution algorithm
	output, err := ApplyMergeRule(context.Background(), applier, input,
		func() string { return misc.FastUUID().String() },
		func() string { return currentTimeString },
	)
	if err != nil {
		return 0, err
	}

	// Step 5: Write mapping rows to gzip file (unchanged from original)
	columnNames := []string{"merge_property_type", "merge_property_value", "rudder_id", "updated_at"}
	for _, row := range output.MappingRows {
		eventLoader := idr.encodingFactory.NewEventLoader(gzWriter, idr.uploader.GetLoadFileType(), idr.warehouse.Type, nil)
		// TODO : support add row for parquet loader
		rowData := []string{row.MergePropertyType, row.MergePropertyValue, row.RudderID, row.UpdatedAt}
		eventLoader.AddRow(columnNames, rowData)
		data, _ := eventLoader.WriteToString()
		_ = gzWriter.WriteGZ(data)
	}

	return len(output.MappingRows), nil
}

// addRules loads merge rules from gzipped load files into a staging table, deduplicates against
// the existing merge rules table, writes new rules to the output gzip file, and inserts them
// into the permanent merge rules table. This method currently operates on the two-property
// warehouse model (merge_property_1_type/value, merge_property_2_type/value). The flexible
// MergeRule model can be used to extend this to support arbitrary property counts in the future.
func (idr *Identity) addRules(txn *sqlmiddleware.Tx, loadFileNames []string, gzWriter *misc.GZipWriter) (ids []int64, err error) {
	// add rules from load files into temp table
	// use original table to delete redundant ones from temp table
	// insert from temp table into original table
	mergeRulesStagingTable := fmt.Sprintf(`rudder_identity_merge_rules_staging_%s`, warehouseutils.RandHex())
	sqlStatement := fmt.Sprintf(`CREATE TEMP TABLE %s
						ON COMMIT DROP
						AS SELECT * FROM %s
						WITH NO DATA;`, mergeRulesStagingTable, idr.mergeRulesTable())

	pkgLogger.Infon("IDR: Creating temp table in postgres for loading data",
		logger.NewStringField("tempTable", mergeRulesStagingTable),
		logger.NewStringField("sourceTable", idr.mergeRulesTable()),
		logger.NewStringField(logfield.Query, sqlStatement),
	)
	_, err = txn.Exec(sqlStatement)
	if err != nil {
		pkgLogger.Errorn("IDR: Error creating temp table in postgres",
			logger.NewStringField("tempTable", mergeRulesStagingTable),
			obskit.Error(err),
		)
		return ids, err
	}

	sortedColumnNames := []string{"merge_property_1_type", "merge_property_1_value", "merge_property_2_type", "merge_property_2_value", "id"}
	stmt, err := txn.Prepare(pq.CopyIn(mergeRulesStagingTable, sortedColumnNames...))
	if err != nil {
		pkgLogger.Errorn("IDR: Error starting bulk copy using CopyIn",
			obskit.Error(err),
		)
		return ids, err
	}
	defer func() {
		_ = stmt.Close()
	}()

	var rowID int

	for _, loadFileName := range loadFileNames {
		var gzipFile *os.File
		gzipFile, err = os.Open(loadFileName)
		if err != nil {
			pkgLogger.Errorn("IDR: Error opening downloaded load file",
				logger.NewStringField("loadFileName", loadFileName),
				obskit.Error(err),
			)
			return ids, err
		}
		defer gzipFile.Close()

		var gzipReader *gzip.Reader
		gzipReader, err = gzip.NewReader(gzipFile)
		if err != nil {
			pkgLogger.Errorn("IDR: Error reading downloaded load file",
				logger.NewStringField("loadFileName", loadFileName),
				obskit.Error(err),
			)
			return ids, err
		}
		defer gzipReader.Close()

		eventReader := idr.encodingFactory.NewEventReader(gzipReader, idr.warehouse.Type)
		columnNames := []string{"merge_property_1_type", "merge_property_1_value", "merge_property_2_type", "merge_property_2_value"}
		for {
			var record []string
			record, err = eventReader.Read(columnNames)
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				pkgLogger.Errorn("IDR: Error while reading merge rule file for loading in staging table locally",
					logger.NewStringField("loadFileName", loadFileName),
					logger.NewStringField("stagingTable", mergeRulesStagingTable),
					obskit.Error(err),
				)
				return ids, err
			}
			var recordInterface [5]any
			for idx, value := range record {
				if strings.TrimSpace(value) != "" {
					recordInterface[idx] = value
				}
			}
			// add rowID which allows us to insert in same order from staging to original merge _rules table
			rowID++
			recordInterface[4] = rowID
			_, err = stmt.Exec(recordInterface[:]...)
			if err != nil {
				pkgLogger.Errorn("IDR: Error while adding rowID to merge_rules table",
					obskit.Error(err),
				)
				return ids, err
			}
		}
	}

	_, err = stmt.Exec()
	if err != nil {
		pkgLogger.Errorn("IDR: Error bulk copy using CopyIn",
			obskit.Error(err),
			obskit.UploadID(idr.uploadID),
		)
		return ids, err
	}

	sqlStatement = fmt.Sprintf(`DELETE FROM %s AS staging
					USING %s original
					WHERE
					(original.merge_property_1_type = staging.merge_property_1_type)
					AND
					(original.merge_property_1_value = staging.merge_property_1_value)
					AND
					(original.merge_property_2_type = staging.merge_property_2_type)
					AND
					(original.merge_property_2_value = staging.merge_property_2_value)`,
		mergeRulesStagingTable, idr.mergeRulesTable())
	pkgLogger.Infon("IDR: Deleting from staging table using source table",
		logger.NewStringField("stagingTable", mergeRulesStagingTable),
		logger.NewStringField("sourceTable", idr.mergeRulesTable()),
		logger.NewStringField(logfield.Query, sqlStatement),
	)
	_, err = txn.Exec(sqlStatement)
	if err != nil {
		pkgLogger.Errorn("IDR: Error deleting from staging table using source table",
			logger.NewStringField("stagingTable", mergeRulesStagingTable),
			logger.NewStringField("sourceTable", idr.mergeRulesTable()),
			obskit.Error(err),
		)
		return ids, err
	}

	// write merge rules to file to be uploaded to warehouse in later steps
	err = idr.writeTableToFile(mergeRulesStagingTable, txn, gzWriter)
	if err != nil {
		pkgLogger.Errorn("IDR: Error writing staging table to file",
			logger.NewStringField("stagingTable", mergeRulesStagingTable),
			obskit.Error(err),
		)
		return ids, err
	}

	// select and insert distinct combination of merge rules and sort them by order in which they were added
	sqlStatement = fmt.Sprintf(`INSERT INTO %s
						(merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value)
						SELECT merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value FROM
						(
							SELECT DISTINCT ON (
								merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value
							) id, merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value
							FROM %s
						) t
		 				ORDER BY id ASC RETURNING id`, idr.mergeRulesTable(), mergeRulesStagingTable)
	pkgLogger.Infon("IDR: Inserting into target table from staging table",
		logger.NewStringField("targetTable", idr.mergeRulesTable()),
		logger.NewStringField("stagingTable", mergeRulesStagingTable),
		logger.NewStringField(logfield.Query, sqlStatement),
	)
	rows, err := txn.Query(sqlStatement)
	if err != nil {
		pkgLogger.Errorn("IDR: Error inserting into target table from staging table",
			logger.NewStringField("targetTable", idr.mergeRulesTable()),
			logger.NewStringField("stagingTable", mergeRulesStagingTable),
			obskit.Error(err),
		)
		return ids, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		err = rows.Scan(&id)
		if err != nil {
			pkgLogger.Errorn("IDR: Error reading id from inserted column",
				logger.NewStringField("targetTable", idr.mergeRulesTable()),
				logger.NewStringField("stagingTable", mergeRulesStagingTable),
				obskit.Error(err),
			)
			return ids, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		pkgLogger.Errorn("IDR: Error reading rows",
			logger.NewStringField("targetTable", idr.mergeRulesTable()),
			logger.NewStringField("stagingTable", mergeRulesStagingTable),
			obskit.Error(err),
		)
		return ids, err
	}
	pkgLogger.Debugn("IDR: Number of merge rules inserted for uploadID", logger.NewIntField("uploadID", idr.uploadID), logger.NewIntField("count", int64(len(ids))))
	return ids, nil
}

// NewMergeRuleFromWarehouse creates a MergeRule from the warehouse two-property model.
// This bridges the fixed-column warehouse schema (merge_property_1_type/value,
// merge_property_2_type/value) to the flexible MergeRule model used by the shared
// resolution algorithm and the real-time identity graph.
func NewMergeRuleFromWarehouse(prop1Type, prop1Value string, prop2Type, prop2Value sql.NullString) MergeRule {
	rule := MergeRule{}
	if prop1Type != "" && prop1Value != "" {
		rule.Properties = append(rule.Properties, MergeProperty{Type: prop1Type, Value: prop1Value})
	}
	if prop2Type.Valid && prop2Value.Valid && prop2Type.String != "" && prop2Value.String != "" {
		rule.Properties = append(rule.Properties, MergeProperty{Type: prop2Type.String, Value: prop2Value.String})
	}
	return rule
}

func (idr *Identity) writeTableToFile(tableName string, txn *sqlmiddleware.Tx, gzWriter *misc.GZipWriter) (err error) {
	batchSize := int64(500)
	sqlStatement := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tableName)
	var totalRows int64
	err = txn.QueryRow(sqlStatement).Scan(&totalRows)
	if err != nil {
		return err
	}

	var offset int64
	for {
		sqlStatement = fmt.Sprintf(`SELECT merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value FROM %s LIMIT %d OFFSET %d`, tableName, batchSize, offset)

		var rows *sqlmiddleware.Rows
		rows, err = txn.Query(sqlStatement)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		columnNames := []string{"merge_property_1_type", "merge_property_1_value", "merge_property_2_type", "merge_property_2_value"}
		for rows.Next() {
			var rowData []string
			eventLoader := idr.encodingFactory.NewEventLoader(gzWriter, idr.uploader.GetLoadFileType(), idr.warehouse.Type, nil)
			var prop1Val, prop2Val, prop1Type, prop2Type sql.NullString
			err = rows.Scan(
				&prop1Type,
				&prop1Val,
				&prop2Type,
				&prop2Val,
			)
			if err != nil {
				return err
			}
			rowData = append(rowData, prop1Type.String, prop1Val.String, prop2Type.String, prop2Val.String)
			for i, columnName := range columnNames {
				// TODO : use proper column type here
				eventLoader.AddColumn(columnName, "", rowData[i])
			}
			rowString, _ := eventLoader.WriteToString()
			_ = gzWriter.WriteGZ(rowString)
		}
		if err = rows.Err(); err != nil {
			return err
		}

		offset += batchSize
		if offset >= totalRows {
			break
		}
	}
	return err
}

func (idr *Identity) uploadFile(ctx context.Context, filePath string, txn *sqlmiddleware.Tx, tableName string, totalRecords int) (err error) {
	outputFile, err := os.Open(filePath)
	if err != nil {
		panic(err)
	}
	storageProvider := warehouseutils.ObjectStorageType(idr.warehouse.Destination.DestinationDefinition.Name, idr.warehouse.Destination.Config, idr.uploader.UseRudderStorage())
	uploader, err := filemanager.New(&filemanager.Settings{
		Provider: storageProvider,
		Config: misc.GetObjectStorageConfig(misc.ObjectStorageOptsT{
			Provider:         storageProvider,
			Config:           idr.warehouse.Destination.Config,
			UseRudderStorage: idr.uploader.UseRudderStorage(),
		}),
		Conf: config.Default,
	})
	if err != nil {
		pkgLogger.Errorn("IDR: Error in creating a file manager",
			logger.NewStringField("destinationName", idr.warehouse.Destination.DestinationDefinition.Name),
			obskit.Error(err),
		)
		return err
	}
	output, err := uploader.Upload(ctx, outputFile, config.GetString("WAREHOUSE_BUCKET_LOAD_OBJECTS_FOLDER_NAME", "rudder-warehouse-load-objects"), tableName, idr.warehouse.Source.ID, tableName)
	if err != nil {
		return err
	}

	sqlStatement := fmt.Sprintf(`UPDATE %s SET location='%s', total_events=%d WHERE wh_upload_id=%d AND table_name='%s'`, warehouseutils.WarehouseTableUploadsTable, output.Location, totalRecords, idr.uploadID, warehouseutils.ToProviderCase(idr.warehouse.Destination.DestinationDefinition.Name, tableName))
	pkgLogger.Infon("IDR: Updating load file location for table",
		logger.NewStringField(logfield.TableName, tableName),
		logger.NewStringField(logfield.Query, sqlStatement),
	)
	_, err = txn.Exec(sqlStatement)
	if err != nil {
		pkgLogger.Errorn("IDR: Error updating load file location for table",
			logger.NewStringField(logfield.TableName, tableName),
			obskit.Error(err),
		)
	}
	return err
}

func (idr *Identity) createTempGzFile(dirName string) (gzWriter misc.GZipWriter, path string) {
	tmpDirPath, err := misc.GetTmpDir()
	if err != nil {
		panic(err)
	}
	fileExtension := warehouseutils.GetTempFileExtension(idr.warehouse.Type)
	path = tmpDirPath + dirName + fmt.Sprintf(`%s_%s/%v/`, idr.warehouse.Destination.DestinationDefinition.Name, idr.warehouse.Destination.ID, idr.uploadID) + misc.FastUUID().String() + "." + fileExtension
	err = os.MkdirAll(filepath.Dir(path), os.ModePerm)
	if err != nil {
		panic(err)
	}
	gzWriter, err = misc.CreateGZ(path)
	if err != nil {
		panic(err)
	}
	return gzWriter, path
}

func (idr *Identity) processMergeRules(ctx context.Context, fileNames []string) (err error) {
	txn, err := idr.db.BeginTx(ctx, nil)
	if err != nil {
		panic(err)
	}

	// START: Add new merge rules to local pg table and also to file
	mergeRulesFileGzWriter, mergeRulesFilePath := idr.createTempGzFile(fmt.Sprintf(`/%s/`, misc.RudderIdentityMergeRulesTmp))
	defer misc.RemoveFilePaths(mergeRulesFilePath)

	ruleIDs, err := idr.addRules(txn, fileNames, &mergeRulesFileGzWriter)
	if err != nil {
		pkgLogger.Errorn("IDR: Error adding rules to table",
			logger.NewStringField(logfield.TableName, idr.mergeRulesTable()),
			obskit.Error(err),
		)
		return err
	}
	_ = mergeRulesFileGzWriter.CloseGZ()
	pkgLogger.Infon("IDR: Added unique rules to table and file",
		logger.NewIntField("ruleCount", int64(len(ruleIDs))),
		logger.NewStringField(logfield.TableName, idr.mergeRulesTable()),
	)
	// END: Add new merge rules to local pg table and also to file

	// START: Add new/changed identity mappings to local pg table and also to file
	mappingsFileGzWriter, mappingsFilePath := idr.createTempGzFile(fmt.Sprintf(`/%s/`, misc.RudderIdentityMappingsTmp))
	defer misc.RemoveFilePaths(mappingsFilePath)
	var totalMappingRecords int
	for idx, ruleID := range ruleIDs {
		var count int
		count, err = idr.applyRule(txn, ruleID, &mappingsFileGzWriter)
		if err != nil {
			pkgLogger.Errorn("IDR: Error applying rule in table",
				logger.NewIntField("ruleID", ruleID),
				logger.NewStringField(logfield.TableName, idr.mergeRulesTable()),
				obskit.Error(err),
			)
			return err
		}
		totalMappingRecords += count
		if idx%1000 == 0 {
			pkgLogger.Infon("IDR: Applied rules progress",
				logger.NewIntField("rulesApplied", int64(idx+1)),
				logger.NewIntField("totalRules", int64(len(ruleIDs))),
				logger.NewIntField("totalMappingRecords", int64(totalMappingRecords)),
				logger.NewStringField(logfield.Namespace, idr.warehouse.Namespace),
				logger.NewStringField(logfield.DestinationType, idr.warehouse.Type),
				logger.NewStringField(logfield.DestinationID, idr.warehouse.Destination.ID),
			)
		}
	}
	_ = mappingsFileGzWriter.CloseGZ()
	// END: Add new/changed identity mappings to local pg table and also to file

	// upload new merge rules to object storage
	err = idr.uploadFile(ctx, mergeRulesFilePath, txn, idr.whMergeRulesTable(), len(ruleIDs))
	if err != nil {
		pkgLogger.Errorn("IDR: Error uploading load file to object storage",
			logger.NewStringField(logfield.TableName, idr.mergeRulesTable()),
			logger.NewStringField("filePath", mergeRulesFilePath),
			obskit.Error(err),
		)
		return err
	}

	// upload new/changed identity mappings to object storage
	err = idr.uploadFile(ctx, mappingsFilePath, txn, idr.whMappingsTable(), totalMappingRecords)
	if err != nil {
		pkgLogger.Errorn("IDR: Error uploading load file to object storage",
			logger.NewStringField("mappingsFilePath", mappingsFilePath),
			logger.NewStringField("mergeRulesFilePath", mergeRulesFilePath),
			obskit.Error(err),
		)
		return err
	}

	err = txn.Commit()
	if err != nil {
		pkgLogger.Errorn("IDR: Error committing transaction",
			obskit.Error(err),
		)
		return err
	}
	return err
}

// Resolve does the below things in a single pg txn
// 1. Fetch all new merge rules added in the upload
// 2. Append to local identity merge rules table
// 3. Apply each merge rule and update local identity mapping table
// 4. Upload the diff of each table to load files for both tables
func (idr *Identity) Resolve(ctx context.Context) (err error) {
	var loadFileNames []string
	defer misc.RemoveFilePaths(loadFileNames...)
	loadFileNames, err = idr.downloader.Download(ctx, idr.whMergeRulesTable())
	if err != nil {
		pkgLogger.Errorn("IDR: Failed to download load files",
			logger.NewStringField(logfield.TableName, idr.mergeRulesTable()), obskit.Error(err))
		return err
	}

	return idr.processMergeRules(ctx, loadFileNames)
}

func (idr *Identity) ResolveHistoricIdentities(ctx context.Context) (err error) {
	var loadFileNames []string
	defer misc.RemoveFilePaths(loadFileNames...)
	gzWriter, path := idr.createTempGzFile(fmt.Sprintf(`/%s/`, misc.RudderIdentityMergeRulesTmp))
	err = idr.warehouseManager.DownloadIdentityRules(ctx, &gzWriter)
	_ = gzWriter.CloseGZ()
	if err != nil {
		pkgLogger.Errorn("IDR: Failed to download identity information from warehouse", obskit.Error(err))
		return err
	}
	loadFileNames = append(loadFileNames, path)

	return idr.processMergeRules(ctx, loadFileNames)
}
