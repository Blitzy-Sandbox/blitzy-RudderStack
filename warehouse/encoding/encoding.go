package encoding

import (
	"io"
	"os"

	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/warehouse/internal/model"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"

	"github.com/rudderlabs/rudder-go-kit/config"
)

const (
	UUIDTsColumn     = "uuid_ts"
	LoadedAtColumn   = "loaded_at"
	BQLoadedAtFormat = "2006-01-02 15:04:05.999999 Z"
	BQUuidTSFormat   = "2006-01-02 15:04:05 Z"
)

type Factory struct {
	config struct {
		maxStagingFileReadBufferCapacityInK int
		parquetParallelWriters              config.ValueLoader[int64]
		disableParquetColumnIndex           config.ValueLoader[bool]
	}
}

func NewFactory(conf *config.Config) *Factory {
	m := &Factory{}

	m.config.maxStagingFileReadBufferCapacityInK = conf.GetIntVar(10240, 1, "Warehouse.maxStagingFileReadBufferCapacityInK")
	m.config.parquetParallelWriters = conf.GetReloadableInt64Var(8, 1, "Warehouse.parquetParallelWriters")
	m.config.disableParquetColumnIndex = conf.GetReloadableBoolVar(true, "Warehouse.disableParquetColumnIndex")
	return m
}

// LoadFileWriter is an interface for writing events to a load file
type LoadFileWriter interface {
	WriteGZ(s string) error
	Write(p []byte) (int, error)
	WriteRow(r []any) error
	Close() error
	GetLoadFile() *os.File
}

func (m *Factory) NewLoadFileWriter(loadFileType, outputFilePath string, schema model.TableSchema, destType string) (LoadFileWriter, error) {
	switch loadFileType {
	case warehouseutils.LoadFileTypeParquet:
		return createParquetWriter(outputFilePath, schema, destType, m.config.parquetParallelWriters.Load(), m.config.disableParquetColumnIndex.Load())
	default:
		return misc.CreateGZ(outputFilePath)
	}
}

// EventLoader is an interface for loading events into a load file
// It's used to load singular BatchRouterEvent events into a load file
type EventLoader interface {
	IsLoadTimeColumn(columnName string) bool
	GetLoadTimeFormat(columnName string) string
	AddColumn(columnName, columnType string, val any)
	AddRow(columnNames, values []string)
	AddEmptyColumn(columnName string)
	WriteToString() (string, error)
	Write() error
}

func (m *Factory) NewEventLoader(w LoadFileWriter, loadFileType, destinationType string, excludedColumns []string) EventLoader {
	var loader EventLoader
	switch loadFileType {
	case warehouseutils.LoadFileTypeJson:
		loader = newJSONLoader(w, destinationType)
	case warehouseutils.LoadFileTypeParquet:
		loader = newParquetLoader(w, destinationType)
	default:
		loader = newCSVLoader(w, destinationType)
	}

	if len(excludedColumns) > 0 {
		exclusionSet := make(map[string]struct{}, len(excludedColumns))
		for _, col := range excludedColumns {
			exclusionSet[col] = struct{}{}
		}
		return &filteringEventLoader{
			inner:           loader,
			excludedColumns: exclusionSet,
		}
	}
	return loader
}

// filteringEventLoader wraps an EventLoader and filters out excluded columns.
// When a column name matches an entry in the exclusion set, the AddColumn,
// AddEmptyColumn, and AddRow calls for that column are silently skipped.
// This is used by the selective sync feature (E-034) to exclude specific
// columns from warehouse load files during encoding.
type filteringEventLoader struct {
	inner           EventLoader
	excludedColumns map[string]struct{}
}

func (f *filteringEventLoader) IsLoadTimeColumn(columnName string) bool {
	return f.inner.IsLoadTimeColumn(columnName)
}

func (f *filteringEventLoader) GetLoadTimeFormat(columnName string) string {
	return f.inner.GetLoadTimeFormat(columnName)
}

func (f *filteringEventLoader) AddColumn(columnName, columnType string, val any) {
	if _, excluded := f.excludedColumns[columnName]; excluded {
		// Insert a null placeholder so positional loaders (Parquet, CSV) keep
		// columns aligned with the schema.  Map-based loaders (JSON) will emit
		// a null key, which readers return as "".
		f.inner.AddEmptyColumn(columnName)
		return
	}
	f.inner.AddColumn(columnName, columnType, val)
}

func (f *filteringEventLoader) AddRow(columnNames, values []string) {
	filteredNames := make([]string, 0, len(columnNames))
	filteredValues := make([]string, 0, len(values))
	for i, name := range columnNames {
		if _, excluded := f.excludedColumns[name]; !excluded {
			filteredNames = append(filteredNames, name)
			if i < len(values) {
				filteredValues = append(filteredValues, values[i])
			}
		}
	}
	f.inner.AddRow(filteredNames, filteredValues)
}

func (f *filteringEventLoader) AddEmptyColumn(columnName string) {
	// Always pass through to the inner loader — even for excluded columns —
	// so that positional loaders (Parquet, CSV) maintain schema alignment.
	// The inner loader writes a null/empty placeholder which readers return as "".
	f.inner.AddEmptyColumn(columnName)
}

func (f *filteringEventLoader) WriteToString() (string, error) {
	return f.inner.WriteToString()
}

func (f *filteringEventLoader) Write() error {
	return f.inner.Write()
}

// EventReader is an interface for reading events from a load file
type EventReader interface {
	Read(columnNames []string) (record []string, err error)
}

func (m *Factory) NewEventReader(r io.Reader, destType string) EventReader {
	switch destType {
	case warehouseutils.BQ:
		return newJSONReader(r, m.config.maxStagingFileReadBufferCapacityInK)
	default:
		return newCsvReader(r)
	}
}
