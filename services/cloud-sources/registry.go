package cloudsources

import (
	"sort"
	"strings"
	"sync"
)

// ConnectorFactory is a function type that creates new CloudSource instances
// from a given configuration. Connector implementations register their factory
// functions with the registry during init() or application startup.
//
// The factory receives a CloudSourceConfig containing all configuration
// needed to initialize the connector, including credentials, polling/webhook
// settings, and source metadata. It returns a ready-to-use CloudSource
// instance or an error if initialization fails.
//
// Example usage:
//
//	factory := func(cfg CloudSourceConfig) (CloudSource, error) {
//	    return NewStripeConnector(cfg)
//	}
//	DefaultRegistry.Register("stripe", factory)
type ConnectorFactory func(cfg CloudSourceConfig) (CloudSource, error)

// Registry is a thread-safe registry of cloud source connector factories.
// It provides registration, lookup, and listing capabilities for managing
// cloud source connector plugins in the ingestion framework.
//
// All methods are safe for concurrent use from multiple goroutines.
// The registry uses sync.RWMutex to allow concurrent reads (Get, List, Len)
// while serializing writes (Register).
//
// Connector names are normalized to lowercase for case-insensitive matching.
// Registering a factory under an existing name silently overwrites the
// previous factory, enabling hot-reload and test override scenarios.
type Registry struct {
	mu         sync.RWMutex
	connectors map[string]ConnectorFactory
}

// NewRegistry creates and returns a new, empty Registry instance with an
// initialized internal connectors map. The returned Registry is ready for
// immediate use — callers can begin registering and looking up connector
// factories without further initialization.
func NewRegistry() *Registry {
	return &Registry{
		connectors: make(map[string]ConnectorFactory),
	}
}

// Register adds a connector factory to the registry under the given name.
// The name is normalized to lowercase for case-insensitive lookup consistency.
// If a factory is already registered under the same name, it is silently
// overwritten with the new factory.
//
// Register acquires a write lock and is safe for concurrent use.
//
// Example:
//
//	registry.Register("stripe", stripeFactory)
//	registry.Register("Salesforce", salesforceFactory) // stored as "salesforce"
func (r *Registry) Register(name string, factory ConnectorFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectors[strings.ToLower(name)] = factory
}

// Get retrieves a connector factory by name from the registry.
// The name is normalized to lowercase for case-insensitive matching.
// Returns the factory and true if found, or nil and false if no factory
// is registered under the given name.
//
// Get acquires a read lock and is safe for concurrent use. Multiple
// goroutines can call Get simultaneously without blocking each other.
//
// Example:
//
//	factory, ok := registry.Get("stripe")
//	if !ok {
//	    return fmt.Errorf("unknown connector: stripe")
//	}
//	source, err := factory(cfg)
func (r *Registry) Get(name string) (ConnectorFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.connectors[strings.ToLower(name)]
	return factory, ok
}

// List returns a sorted slice of all registered connector names.
// The names are returned in alphabetical order for deterministic output,
// which is useful for display, logging, and testing purposes.
//
// List acquires a read lock and is safe for concurrent use.
// The returned slice is a copy — modifications to it do not affect the registry.
//
// Example:
//
//	names := registry.List() // ["hubspot", "salesforce", "stripe"]
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.connectors))
	for name := range r.connectors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Len returns the number of connector factories currently registered
// in the registry. This is a constant-time operation.
//
// Len acquires a read lock and is safe for concurrent use.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.connectors)
}

// DefaultRegistry is the package-level default registry for convenience.
// Connector packages can register themselves during init() to make their
// factories available globally without requiring callers to manage a
// Registry instance explicitly.
//
// Example (in a connector package init function):
//
//	func init() {
//	    cloudsources.DefaultRegistry.Register("stripe", NewStripeConnector)
//	}
var DefaultRegistry = NewRegistry()
