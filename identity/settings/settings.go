// Package settings implements configurable identity resolution rules for the
// RudderStack identity graph (E-030). It controls how identities are resolved
// and merged: blocked values (regex/exact-match), identifier limits
// (weekly/monthly/annually/ever), and priority ranking for external identifier types.
package settings

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
)

var pkgLogger logger.Logger

func init() {
	pkgLogger = logger.NewLogger().Child("identity").Child("settings")
}

// BlockedValueRule represents a rule for blocking specific identifier values
// from being used in identity resolution. Supports exact match and regex patterns.
type BlockedValueRule struct {
	// Type is either "exact" for exact string match or "regex" for regex pattern matching
	Type string `json:"type"`
	// Value is the exact string or regex pattern to match against
	Value string `json:"value"`
}

// IdentifierLimit defines the maximum number of values allowed for a specific
// identifier type per profile within a given time window.
type IdentifierLimit struct {
	// MaxCount is the maximum number of identifier values per profile
	MaxCount int `json:"maxCount"`
	// TimeWindow is one of: "weekly", "monthly", "annually", "ever"
	TimeWindow string `json:"timeWindow"`
}

// IdentifierConfig holds the complete resolution configuration for a single
// external identifier type, including blocking rules, value limits, and priority.
type IdentifierConfig struct {
	// BlockedValues contains rules for values that should never be used for resolution
	BlockedValues []BlockedValueRule `json:"blockedValues"`
	// Limit defines the maximum count and time window for this identifier type
	Limit IdentifierLimit `json:"limit"`
	// Priority determines resolution order (lower number = higher priority)
	Priority int `json:"priority"`
}

// ResolutionSettings manages configurable identity resolution rules including
// blocked values, identifier limits, and priority ranking. Settings are loaded
// from backend-config and use rudder-go-kit/config for reloadable variables.
// Thread-safe for concurrent access via RWMutex (read-heavy workload).
type ResolutionSettings struct {
	mu              sync.RWMutex
	identifiers     map[string]*IdentifierConfig
	compiledRegexes map[string]*regexp.Regexp // cached compiled regexes keyed by pattern
	defaultLimit    IdentifierLimit
	conf            *config.Config
}

// ValidTimeWindows defines the allowed time window values for identifier limits.
var ValidTimeWindows = map[string]bool{
	"weekly":   true,
	"monthly":  true,
	"annually": true,
	"ever":     true,
}

// DefaultExternalIDTypes lists the 12 default external identifier types
// supported by the identity resolution system, matching Segment Unify defaults.
// Sourced from refs/segment-docs/src/unify/identity-resolution/externalids.md
var DefaultExternalIDTypes = []string{
	"user_id",
	"email",
	"anonymous_id",
	"android.id",
	"android.idfa",
	"android.push_token",
	"ios.id",
	"ios.idfa",
	"ios.push_token",
	"ga_client_id",
	"cross_domain_id",
	"braze_id",
}

// GetWindowDuration returns the time.Duration corresponding to a time window string.
// Returns 0 for "ever" (lifetime limit with no window expiry) and unknown values.
func GetWindowDuration(window string) time.Duration {
	switch window {
	case "weekly":
		return 7 * 24 * time.Hour
	case "monthly":
		return 30 * 24 * time.Hour
	case "annually":
		return 365 * 24 * time.Hour
	case "ever":
		return 0
	default:
		return 0
	}
}

// DefaultBlockedValues returns the Segment-recommended blocked value rules
// that prevent common problematic values from corrupting the identity graph.
// Sourced from refs/segment-docs/src/unify/identity-resolution/identity-resolution-settings.md
func DefaultBlockedValues() []BlockedValueRule {
	return []BlockedValueRule{
		{Type: "regex", Value: "^[0-]*$"},  // Zeroes and Dashes
		{Type: "exact", Value: "-1"},        // Sentinel value
		{Type: "exact", Value: "null"},      // Null string
		{Type: "exact", Value: "anonymous"}, // Generic anonymous
	}
}

// New creates a new ResolutionSettings with the given configuration.
// The conf parameter provides access to reloadable configuration variables
// under the "Identity.Resolution" config key prefix.
func New(conf *config.Config) *ResolutionSettings {
	s := &ResolutionSettings{
		identifiers:     make(map[string]*IdentifierConfig),
		compiledRegexes: make(map[string]*regexp.Regexp),
		defaultLimit: IdentifierLimit{
			MaxCount:   5,
			TimeWindow: "monthly",
		},
		conf: conf,
	}
	if conf != nil {
		s.defaultLimit.MaxCount = conf.GetIntVar(5, 1, "Identity.Resolution.defaultLimit.maxCount")
		s.defaultLimit.TimeWindow = conf.GetStringVar("monthly", "Identity.Resolution.defaultLimit.timeWindow")
	}
	pkgLogger.Infon("Identity resolution settings initialized")
	return s
}

// DefaultSettings creates a new ResolutionSettings with Segment-compatible default
// configuration: user_id priority 1 with limit 1 ever, email priority 2 with
// limit 5 annually, all other identifiers with limit 5 monthly and alphabetical
// priority starting from 3.
func DefaultSettings() *ResolutionSettings {
	s := &ResolutionSettings{
		identifiers:     make(map[string]*IdentifierConfig),
		compiledRegexes: make(map[string]*regexp.Regexp),
		defaultLimit:    IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
	}

	// Compile default regex patterns into cache
	defaultBlocked := DefaultBlockedValues()
	for _, rule := range defaultBlocked {
		if rule.Type == "regex" {
			compiled, err := regexp.Compile(rule.Value)
			if err == nil {
				s.compiledRegexes[rule.Value] = compiled
			}
		}
	}

	// user_id: highest priority, immutable (limit 1 ever)
	s.identifiers["user_id"] = &IdentifierConfig{
		BlockedValues: DefaultBlockedValues(),
		Limit:         IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority:      1,
	}

	// email: second priority, limit 5 annually
	s.identifiers["email"] = &IdentifierConfig{
		BlockedValues: DefaultBlockedValues(),
		Limit:         IdentifierLimit{MaxCount: 5, TimeWindow: "annually"},
		Priority:      2,
	}

	// Remaining default identifiers: priority 3+ alphabetically, limit 5 monthly
	remaining := make([]string, 0, len(DefaultExternalIDTypes))
	for _, idType := range DefaultExternalIDTypes {
		if idType != "user_id" && idType != "email" {
			remaining = append(remaining, idType)
		}
	}
	sort.Strings(remaining)

	for i, idType := range remaining {
		s.identifiers[idType] = &IdentifierConfig{
			BlockedValues: DefaultBlockedValues(),
			Limit:         IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
			Priority:      3 + i,
		}
	}

	return s
}

// LoadFromConfig loads identity resolution settings from a configuration map,
// typically sourced from backend-config. This method is thread-safe and replaces
// any existing identifier settings from the config map atomically.
func (s *ResolutionSettings) LoadFromConfig(configMap map[string]any) error {
	identifiersRaw, ok := configMap["identifiers"]
	if !ok {
		return nil // No identifiers configuration present
	}

	identifiersMap, ok := identifiersRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid identifiers config: expected map, got %T", identifiersRaw)
	}

	// Parse and validate all identifiers before applying changes (fail-fast)
	newIdentifiers := make(map[string]*IdentifierConfig, len(identifiersMap))
	newRegexes := make(map[string]*regexp.Regexp)

	for idType, cfgRaw := range identifiersMap {
		normalizedType := strings.TrimSpace(idType)
		cfgMap, ok := cfgRaw.(map[string]any)
		if !ok {
			return fmt.Errorf("identifier %q: expected map, got %T", normalizedType, cfgRaw)
		}

		cfg := &IdentifierConfig{}

		// Parse blocked values
		if bvRaw, exists := cfgMap["blockedValues"]; exists {
			bvSlice, ok := bvRaw.([]any)
			if !ok {
				return fmt.Errorf("identifier %q: blockedValues must be an array", normalizedType)
			}
			for _, bvItem := range bvSlice {
				bvMap, ok := bvItem.(map[string]any)
				if !ok {
					continue
				}
				rule := BlockedValueRule{
					Type:  anyToString(bvMap["type"]),
					Value: anyToString(bvMap["value"]),
				}
				cfg.BlockedValues = append(cfg.BlockedValues, rule)
			}
		}

		// Parse limit
		if limitRaw, exists := cfgMap["limit"]; exists {
			if limitMap, ok := limitRaw.(map[string]any); ok {
				cfg.Limit.MaxCount = anyToInt(limitMap["maxCount"])
				cfg.Limit.TimeWindow = anyToString(limitMap["timeWindow"])
			}
		}

		// Parse priority
		if priorityRaw, exists := cfgMap["priority"]; exists {
			cfg.Priority = anyToInt(priorityRaw)
		}

		// Validate limit
		if cfg.Limit.MaxCount <= 0 {
			return fmt.Errorf("identifier %q: maxCount must be positive, got %d", normalizedType, cfg.Limit.MaxCount)
		}
		if !ValidTimeWindows[cfg.Limit.TimeWindow] {
			return fmt.Errorf("identifier %q: invalid time window %q", normalizedType, cfg.Limit.TimeWindow)
		}

		// Validate priority
		if cfg.Priority <= 0 {
			return fmt.Errorf("identifier %q: priority must be positive, got %d", normalizedType, cfg.Priority)
		}

		// Compile regex patterns
		for _, rule := range cfg.BlockedValues {
			if rule.Type == "regex" {
				compiled, err := regexp.Compile(rule.Value)
				if err != nil {
					return fmt.Errorf("identifier %q: invalid regex pattern %q: %w", normalizedType, rule.Value, err)
				}
				newRegexes[rule.Value] = compiled
			}
		}

		newIdentifiers[normalizedType] = cfg
	}

	// All validation passed — apply atomically under write lock
	s.mu.Lock()
	defer s.mu.Unlock()

	for idType, cfg := range newIdentifiers {
		s.identifiers[idType] = cfg
	}
	for pattern, compiled := range newRegexes {
		s.compiledRegexes[pattern] = compiled
	}

	pkgLogger.Infon("Identity resolution settings loaded from config",
		logger.NewIntField("identifierCount", int64(len(newIdentifiers))))
	return nil
}

// SetIdentifierConfig sets the resolution configuration for a specific external
// identifier type. Validates all rules before applying changes. Thread-safe.
func (s *ResolutionSettings) SetIdentifierConfig(identifierType string, cfg *IdentifierConfig) error {
	// Validate priority
	if cfg.Priority <= 0 {
		return fmt.Errorf("identifier %q: priority must be positive, got %d", identifierType, cfg.Priority)
	}

	// Validate limit
	if cfg.Limit.MaxCount <= 0 {
		return fmt.Errorf("identifier %q: maxCount must be positive, got %d", identifierType, cfg.Limit.MaxCount)
	}
	if !ValidTimeWindows[cfg.Limit.TimeWindow] {
		return fmt.Errorf("identifier %q: invalid time window %q", identifierType, cfg.Limit.TimeWindow)
	}

	// Pre-compile regex patterns (validate before mutation)
	compiledPatterns := make(map[string]*regexp.Regexp)
	for _, rule := range cfg.BlockedValues {
		if rule.Type == "regex" {
			compiled, err := regexp.Compile(rule.Value)
			if err != nil {
				return fmt.Errorf("identifier %q: invalid regex pattern %q: %w", identifierType, rule.Value, err)
			}
			compiledPatterns[rule.Value] = compiled
		}
	}

	// All validation passed — apply changes under write lock
	s.mu.Lock()
	defer s.mu.Unlock()

	s.identifiers[identifierType] = cfg
	for pattern, compiled := range compiledPatterns {
		s.compiledRegexes[pattern] = compiled
	}

	return nil
}

// IsBlocked checks whether a given identifier value is blocked for the specified
// identifier type based on configured exact-match and regex rules. Returns false
// if no rules are configured for the identifier type.
func (s *ResolutionSettings) IsBlocked(identifierType, value string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cfg, exists := s.identifiers[identifierType]
	if !exists || len(cfg.BlockedValues) == 0 {
		return false
	}

	for _, rule := range cfg.BlockedValues {
		switch rule.Type {
		case "exact":
			if rule.Value == value {
				return true
			}
		case "regex":
			if compiled, ok := s.compiledRegexes[rule.Value]; ok {
				if compiled.MatchString(value) {
					return true
				}
			}
		}
	}
	return false
}

// ExceedsLimit checks whether the given count of identifier values exceeds
// the configured limit for the specified identifier type. Uses the default
// limit if no specific limit is configured. count >= MaxCount means exceeded.
func (s *ResolutionSettings) ExceedsLimit(identifierType string, count int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := s.getLimit(identifierType)
	return count >= limit.MaxCount
}

// GetLimit returns the IdentifierLimit for the given identifier type.
// Returns the default limit if no specific limit is configured.
func (s *ResolutionSettings) GetLimit(identifierType string) IdentifierLimit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getLimit(identifierType)
}

// getLimit is the internal implementation (caller must hold at least RLock).
func (s *ResolutionSettings) getLimit(identifierType string) IdentifierLimit {
	if cfg, exists := s.identifiers[identifierType]; exists {
		return cfg.Limit
	}
	return s.defaultLimit
}

// GetPriority returns the resolution priority for the given identifier type.
// Lower numbers indicate higher priority (1 is highest). Returns a computed
// priority based on alphabetical order after configured identifiers for
// unconfigured types.
func (s *ResolutionSettings) GetPriority(identifierType string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getPriority(identifierType)
}

// getPriority is the internal implementation (caller must hold at least RLock).
// For configured identifiers, returns the explicit priority.
// For unconfigured identifiers, computes a deterministic priority that is
// higher than all configured priorities and preserves alphabetical ordering.
func (s *ResolutionSettings) getPriority(identifierType string) int {
	if cfg, exists := s.identifiers[identifierType]; exists {
		return cfg.Priority
	}

	// Compute max configured priority
	maxPriority := 0
	for _, cfg := range s.identifiers {
		if cfg.Priority > maxPriority {
			maxPriority = cfg.Priority
		}
	}

	// For unconfigured identifiers, compute a deterministic offset from the
	// identifier string that preserves lexicographic ordering. Uses the first
	// 4 bytes as a big-endian integer for stable comparison.
	offset := 0
	for i := 0; i < len(identifierType) && i < 4; i++ {
		offset = offset*256 + int(identifierType[i])
	}

	return maxPriority + 1 + offset
}

// CompareIdentifierPriority compares the priority of two identifier types.
// Returns negative if a has higher priority (lower number), positive if b has
// higher priority, and 0 if equal.
func (s *ResolutionSettings) CompareIdentifierPriority(identifierTypeA, identifierTypeB string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getPriority(identifierTypeA) - s.getPriority(identifierTypeB)
}

// ShouldDemoteIdentifier returns true if identifierToCheck should be demoted
// (removed as a resolution identifier) when the higherPriorityIdentifier
// exceeds its limit. The lower-priority identifier (higher number) is demoted.
// Based on Segment's conflict resolution rules.
func (s *ResolutionSettings) ShouldDemoteIdentifier(identifierToCheck, higherPriorityIdentifier string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getPriority(identifierToCheck) > s.getPriority(higherPriorityIdentifier)
}

// GetIdentifierConfig returns the complete IdentifierConfig for the given
// identifier type. Returns a default configuration with nil BlockedValues
// if the identifier type is not explicitly configured.
func (s *ResolutionSettings) GetIdentifierConfig(identifierType string) IdentifierConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if cfg, exists := s.identifiers[identifierType]; exists {
		return *cfg
	}
	// Return default config for unconfigured types
	return IdentifierConfig{
		BlockedValues: nil,
		Limit:         s.defaultLimit,
		Priority:      s.getPriority(identifierType),
	}
}

// Validate checks all resolution settings for correctness:
//   - All regex patterns compile successfully
//   - All time windows are valid ("weekly", "monthly", "annually", "ever")
//   - All MaxCount values are positive (> 0)
//   - All Priority values are positive (> 0)
//
// Returns descriptive errors with identifier type context.
func (s *ResolutionSettings) Validate() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for idType, cfg := range s.identifiers {
		// Validate priority
		if cfg.Priority <= 0 {
			return fmt.Errorf("identifier %q: priority must be positive, got %d", idType, cfg.Priority)
		}

		// Validate limit MaxCount
		if cfg.Limit.MaxCount <= 0 {
			return fmt.Errorf("identifier %q: maxCount must be positive, got %d", idType, cfg.Limit.MaxCount)
		}

		// Validate time window
		if !ValidTimeWindows[cfg.Limit.TimeWindow] {
			return fmt.Errorf("identifier %q: invalid time window %q", idType, cfg.Limit.TimeWindow)
		}

		// Validate regex patterns compile
		for _, rule := range cfg.BlockedValues {
			if rule.Type == "regex" {
				if _, err := regexp.Compile(rule.Value); err != nil {
					return fmt.Errorf("identifier %q: invalid regex pattern %q: %w", idType, rule.Value, err)
				}
			}
		}
	}

	return nil
}

// compileAndCacheRegex compiles a regex pattern and caches the result.
// Returns the cached version if already compiled. Caller must hold write lock.
func (s *ResolutionSettings) compileAndCacheRegex(pattern string) (*regexp.Regexp, error) {
	if compiled, ok := s.compiledRegexes[pattern]; ok {
		return compiled, nil
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern %q: %w", pattern, err)
	}
	s.compiledRegexes[pattern] = compiled
	return compiled, nil
}

// anyToString safely converts an interface{} value to a string.
func anyToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// anyToInt safely converts an interface{} value to an int,
// handling both int and float64 (common in JSON-decoded maps).
func anyToInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	default:
		return 0
	}
}
