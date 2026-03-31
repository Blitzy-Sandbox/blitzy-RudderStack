// Package anomalydetection provides anomaly detection capabilities for the Protocols
// tracking plan enforcement pipeline. It identifies unexpected events and properties
// not defined in tracking plan schemas and tracks their occurrence frequencies over
// configurable sliding time windows.
//
// The package consists of two primary components:
//   - Tracker: frequency tracking over sliding time windows (this file)
//   - Detector: event/property anomaly identification against tracking plan schemas
package anomalydetection

import (
	"sync"
	"time"
)

// TrackerConfig configures the frequency Tracker behavior.
type TrackerConfig struct {
	// TimeWindow is the duration of the sliding time window for frequency tracking.
	// Events older than this window are expired and not counted.
	// Default: 1 hour.
	TimeWindow time.Duration

	// SensitivityThreshold is the minimum number of occurrences within the time window
	// for an anomaly to be flagged as significant. Low values increase sensitivity
	// (more anomalies reported), high values decrease sensitivity.
	// Default: 1 (any occurrence is considered anomalous).
	SensitivityThreshold int
}

// DefaultTrackerConfig returns the default tracker configuration:
//   - TimeWindow: 1 hour
//   - SensitivityThreshold: 1
func DefaultTrackerConfig() TrackerConfig {
	return TrackerConfig{
		TimeWindow:           1 * time.Hour,
		SensitivityThreshold: 1,
	}
}

// occurrence records a single timestamped event or property occurrence.
type occurrence struct {
	timestamp time.Time
}

// trackingKey uniquely identifies a tracked metric: source + event name + optional property name.
type trackingKey struct {
	sourceID     string
	eventName    string
	propertyName string // empty for event-level tracking
}

// Tracker tracks event and property occurrence frequencies over configurable sliding
// time windows. It provides the temporal dimension of anomaly detection, allowing
// differentiation between one-off data quality issues and systematic problems.
//
// Thread-safe: all methods can be called concurrently from multiple processor goroutines.
type Tracker struct {
	mu     sync.RWMutex
	config TrackerConfig

	// occurrences maps a tracking key to a list of timestamped occurrences within the time window.
	// Expired entries are cleaned up lazily during read operations.
	occurrences map[trackingKey][]occurrence
}

// NewTracker creates a new frequency Tracker with the given configuration.
// If config.TimeWindow is zero, it defaults to 1 hour.
// If config.SensitivityThreshold is zero, it defaults to 1.
func NewTracker(config TrackerConfig) *Tracker {
	if config.TimeWindow == 0 {
		config.TimeWindow = 1 * time.Hour
	}
	if config.SensitivityThreshold == 0 {
		config.SensitivityThreshold = 1
	}
	return &Tracker{
		config:      config,
		occurrences: make(map[trackingKey][]occurrence),
	}
}

// RecordEvent records an occurrence of an event name for the given source.
// The occurrence is timestamped with the current time and tracked within the sliding window.
func (t *Tracker) RecordEvent(sourceID, eventName string) {
	key := trackingKey{sourceID: sourceID, eventName: eventName}
	t.record(key)
}

// RecordProperty records an occurrence of a property on an event for the given source.
// Used to track how frequently unexpected properties appear.
func (t *Tracker) RecordProperty(sourceID, eventName, propertyName string) {
	key := trackingKey{sourceID: sourceID, eventName: eventName, propertyName: propertyName}
	t.record(key)
}

// record adds a timestamped occurrence for the given key.
func (t *Tracker) record(key trackingKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.occurrences[key] = append(t.occurrences[key], occurrence{timestamp: time.Now()})
}

// GetEventCount returns the number of times an event name has been recorded for the given
// source within the current time window. Expired entries are pruned.
func (t *Tracker) GetEventCount(sourceID, eventName string) int {
	key := trackingKey{sourceID: sourceID, eventName: eventName}
	return t.getCount(key)
}

// GetPropertyCount returns the number of times a property has been recorded on an event
// for the given source within the current time window. Expired entries are pruned.
func (t *Tracker) GetPropertyCount(sourceID, eventName, propertyName string) int {
	key := trackingKey{sourceID: sourceID, eventName: eventName, propertyName: propertyName}
	return t.getCount(key)
}

// getCount returns the count of non-expired occurrences for the given key.
// Performs lazy cleanup of expired entries.
func (t *Tracker) getCount(key trackingKey) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	entries, ok := t.occurrences[key]
	if !ok {
		return 0
	}

	// Prune expired entries
	cutoff := time.Now().Add(-t.config.TimeWindow)
	valid := entries[:0] // reuse underlying array to avoid allocation
	for _, e := range entries {
		if e.timestamp.After(cutoff) {
			valid = append(valid, e)
		}
	}

	if len(valid) == 0 {
		delete(t.occurrences, key)
		return 0
	}

	t.occurrences[key] = valid
	return len(valid)
}

// IsAnomalous returns true if the occurrence count for the given event exceeds
// the configured sensitivity threshold within the current time window.
// A higher threshold means fewer anomalies are flagged (less sensitive).
func (t *Tracker) IsAnomalous(sourceID, eventName string) bool {
	count := t.GetEventCount(sourceID, eventName)
	return count >= t.config.SensitivityThreshold
}

// IsPropertyAnomalous returns true if the occurrence count for the given property
// exceeds the configured sensitivity threshold within the current time window.
func (t *Tracker) IsPropertyAnomalous(sourceID, eventName, propertyName string) bool {
	count := t.GetPropertyCount(sourceID, eventName, propertyName)
	return count >= t.config.SensitivityThreshold
}

// SetSensitivityThreshold updates the sensitivity threshold at runtime.
// Thread-safe. Values less than or equal to zero are ignored.
func (t *Tracker) SetSensitivityThreshold(threshold int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if threshold > 0 {
		t.config.SensitivityThreshold = threshold
	}
}

// Reset clears all tracked frequency data. Useful for testing or when backend-config
// changes require a fresh baseline.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.occurrences = make(map[trackingKey][]occurrence)
}
