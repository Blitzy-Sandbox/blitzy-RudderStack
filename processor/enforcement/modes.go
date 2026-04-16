// Package enforcement defines the tracking plan enforcement modes for the RudderStack
// processor pipeline. Enforcement modes control how events that violate a tracking plan
// are handled: Block (reject the entire event), Omit (strip non-conforming properties),
// or Allow (log the violation but pass through unchanged).
//
// This package is a pure type and logic package with no external dependencies.
// It provides:
//   - The Mode type and three mode constants (ModeBlock, ModeOmit, ModeAllow)
//   - Behavioral helper functions that callers use to determine event handling
//   - Mode resolution logic supporting per-source and per-call-type configuration
//   - Config extraction helpers for MergedTpConfig integration
//
// Modes are configurable per source and per call type within a source (track, identify,
// group, page, screen). Per-call-type overrides take precedence over source-level defaults.
//
// The zero value of Mode ("") represents no enforcement — the legacy behavior where the
// binary propagateValidationErrors toggle governs whether violation errors are added to
// event context. This ensures full backward compatibility with existing pipeline behavior.
package enforcement

// Mode defines the tracking plan enforcement behavior for events that violate the tracking plan.
// Modes control how violations are handled in the processor pipeline.
//
// The zero value ("") represents no enforcement — the legacy behavior where the binary
// propagateValidationErrors toggle governs whether violation errors are added to event context.
type Mode string

const (
	// ModeBlock rejects the entire event from proceeding through the pipeline.
	// Blocked events are counted in metrics and can optionally be forwarded to an
	// alternative source for debugging via the Forwarder (E-023).
	ModeBlock Mode = "block"

	// ModeOmit strips non-conforming properties from the event but allows the event
	// to proceed through the pipeline. The event's context is enriched with violation
	// information (property names that were removed). Only properties that violate
	// the tracking plan schema are omitted; conforming properties are preserved.
	ModeOmit Mode = "omit"

	// ModeAllow logs the violation and emits metrics but passes the event through
	// the pipeline unchanged. This is equivalent to the existing behavior when
	// propagateValidationErrors is set to "true" in the tracking plan config.
	ModeAllow Mode = "allow"
)

// AllModes contains all valid enforcement modes for validation and iteration.
// This slice can be used to enumerate modes for configuration UI, documentation,
// or validation purposes.
var AllModes = []Mode{ModeBlock, ModeOmit, ModeAllow}

// SupportedCallTypes lists all RudderStack event types that support per-call-type
// enforcement mode overrides. Each of these event types can have an independent
// enforcement mode that overrides the global source-level mode.
var SupportedCallTypes = []string{
	"track",
	"identify",
	"group",
	"page",
	"screen",
}

// IsValidMode returns true if the given mode is one of the three valid enforcement modes
// (Block, Omit, Allow). An empty mode ("") is NOT considered valid — it represents
// the absence of enforcement configuration (legacy behavior).
//
// This function is used to validate mode values from configuration and to guard against
// misconfiguration. Only valid modes trigger enforcement behavior in the pipeline.
func IsValidMode(m Mode) bool {
	switch m {
	case ModeBlock, ModeOmit, ModeAllow:
		return true
	default:
		return false
	}
}

// ShouldRejectEvent returns true if the given enforcement mode requires the entire event
// to be rejected from proceeding through the pipeline.
//
// Returns false for empty mode ("") — no enforcement means no rejection.
// Returns true only for ModeBlock.
//
// Callers in trackingplan.go use this to determine whether a violating event should be
// dropped from the pipeline entirely (with optional forwarding to an alternative source).
func ShouldRejectEvent(m Mode) bool {
	return m == ModeBlock
}

// ShouldStripProperties returns true if the given enforcement mode requires non-conforming
// properties to be removed from the event while allowing the event to proceed.
//
// Returns false for empty mode ("") — no enforcement means no stripping.
// Returns true only for ModeOmit.
//
// Callers in trackingplan.go use this to determine whether non-conforming properties
// should be removed from the event payload before continuing through the pipeline.
// The event's context is enriched with the names of removed properties.
func ShouldStripProperties(m Mode) bool {
	return m == ModeOmit
}

// ShouldLogViolation returns true if the given enforcement mode requires violation
// information to be logged and metrics to be emitted.
//
// Returns false for empty mode ("") — no enforcement means no violation logging
// (the legacy propagateValidationErrors toggle handles this case separately).
// Returns true for all three valid modes (Block, Omit, Allow).
//
// All enforcement modes log violations — the difference is in what action is taken:
// Block rejects the event, Omit strips properties, Allow just logs. But all three modes
// require the violation to be recorded for observability.
func ShouldLogViolation(m Mode) bool {
	return IsValidMode(m)
}

// ResolveMode determines the effective enforcement mode for an event by checking:
//  1. Per-call-type override for the event's type (track, identify, group, page, screen)
//  2. Global source-level mode as fallback
//
// If a per-call-type override exists for the given eventType and it is a valid mode,
// it takes precedence over the global mode. Invalid per-call-type overrides are ignored
// to prevent misconfiguration from affecting pipeline behavior.
//
// If no override exists or eventType is empty, the global mode is returned.
//
// If both globalMode and per-call-type override are empty, returns empty Mode
// (no enforcement — backward compatible with legacy behavior).
//
// Parameters:
//   - globalMode: the source-level default enforcement mode
//   - perCallType: map of event type -> enforcement mode overrides (may be nil)
//   - eventType: the RudderStack event type of the current event (e.g., "track", "identify")
func ResolveMode(globalMode Mode, perCallType map[string]Mode, eventType string) Mode {
	// Check per-call-type override first (higher priority)
	if eventType != "" && perCallType != nil {
		if override, ok := perCallType[eventType]; ok && IsValidMode(override) {
			return override
		}
	}
	// Fall back to global mode
	return globalMode
}

// ResolveModeFromConfig extracts the enforcement mode from the tracking plan's MergedTpConfig.
// This is the primary integration point between the backend-config enforcement settings
// and the runtime enforcement decisions in the processor pipeline.
//
// The function reads:
//   - "enforcementMode" key for the global enforcement mode
//   - "enforcementMode_<eventType>" keys for per-call-type overrides
//
// When "enforcementMode" is not present in the config, returns empty Mode (no enforcement),
// maintaining backward compatibility with the legacy propagateValidationErrors toggle.
//
// The config parameter is typically event.Metadata.MergedTpConfig (map[string]any).
// The eventType parameter is typically event.Metadata.EventType (e.g., "track", "identify").
//
// Parameters:
//   - config: the MergedTpConfig map from event metadata (map[string]any)
//   - eventType: the RudderStack event type for per-call-type resolution
func ResolveModeFromConfig(config map[string]any, eventType string) Mode {
	if config == nil {
		return ""
	}

	// Read global enforcement mode from config
	var globalMode Mode
	if v, ok := config["enforcementMode"]; ok {
		if s, ok := v.(string); ok {
			globalMode = Mode(s)
		}
	}

	// Read per-call-type override if event type is specified
	var perCallType map[string]Mode
	if eventType != "" {
		overrideKey := "enforcementMode_" + eventType
		if v, ok := config[overrideKey]; ok {
			if s, ok := v.(string); ok {
				overrideMode := Mode(s)
				if IsValidMode(overrideMode) {
					perCallType = map[string]Mode{eventType: overrideMode}
				}
			}
		}
	}

	return ResolveMode(globalMode, perCallType, eventType)
}

// GetForwardSourceID extracts the forward-blocked-events source ID from the MergedTpConfig.
// Returns empty string if not configured or if the config is nil.
//
// This corresponds to the TrackingPlanEnforcementConfig.ForwardBlockedEventsSourceID field
// in backend-config/types.go. When a non-empty source ID is returned, blocked events
// (those rejected by ModeBlock enforcement) should be forwarded to this alternative source
// for debugging and analysis purposes.
//
// Parameters:
//   - config: the MergedTpConfig map from event metadata (map[string]any)
func GetForwardSourceID(config map[string]any) string {
	if config == nil {
		return ""
	}
	if v, ok := config["forwardBlockedEventsSourceId"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
