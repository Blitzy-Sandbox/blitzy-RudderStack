// Package graph — externalids.go implements external ID management (E-028).
//
// This file processes context.externalId from incoming events to extract and
// register identifier associations in the identity graph. It defines the
// IdentifierPair type, the default external ID types list, and extraction,
// filtering, and sorting utilities used by the IdentityGraph.ProcessEvent flow.
package graph

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/rudderlabs/rudder-server/identity/settings"
)

// IdentifierPair represents a single external identifier association.
// Type is the identifier type (e.g., "user_id", "email", "ios.idfa")
// and Value is the identifier value.
type IdentifierPair struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// IsEmpty returns true if either Type or Value is empty or whitespace-only.
func (p IdentifierPair) IsEmpty() bool {
	return strings.TrimSpace(p.Type) == "" || strings.TrimSpace(p.Value) == ""
}

// Key returns a unique string key for this identifier pair for deduplication.
func (p IdentifierPair) Key() string {
	return p.Type + ":" + p.Value
}

// DefaultExternalIDTypes lists the 14 default external identifier types
// supported by the identity resolution system, matching Segment Unify defaults.
// Custom types are also supported — this list represents the well-known defaults
// per refs/segment-docs/src/unify/identity-resolution/externalids.md.
var DefaultExternalIDTypes = []string{
	"user_id",
	"email",
	"anonymous_id",
	"ios.id",
	"ios.idfa",
	"ios.push_token",
	"android.id",
	"android.aaid",
	"android.push_token",
	"ga_client_id",
	"cross_domain_id",
	"braze_id",
	"mailchimp_id",
	"amp_id",
}

// defaultExternalIDTypeSet provides O(1) lookup for known external ID types.
var defaultExternalIDTypeSet map[string]struct{}

func init() {
	defaultExternalIDTypeSet = make(map[string]struct{}, len(DefaultExternalIDTypes))
	for _, t := range DefaultExternalIDTypes {
		defaultExternalIDTypeSet[t] = struct{}{}
	}
}

// IsKnownExternalIDType returns true if the given type is one of the default
// external identifier types. Custom types are still supported by the system;
// this only checks against the well-known defaults.
func IsKnownExternalIDType(idType string) bool {
	_, exists := defaultExternalIDTypeSet[idType]
	return exists
}

// ExtractExternalIDs extracts all external identifier pairs from a RudderStack event JSON.
// It collects identifiers from four sources:
//  1. Top-level userId → IdentifierPair{Type: "user_id", Value: ...}
//  2. Top-level anonymousId → IdentifierPair{Type: "anonymous_id", Value: ...}
//  3. traits.email (for identify events) → IdentifierPair{Type: "email", Value: ...}
//  4. context.externalId array → each entry's {type, id} pair
//
// The context.externalId field follows the pattern used in:
// router/batchrouter/asyncdestinationmanager/salesforce-bulk-upload/salesforce_bulk.go:84
//
//	gjson.GetBytes(job.EventPayload, "context.externalId")
//
// Returns deduplicated, non-empty identifier pairs.
func ExtractExternalIDs(eventJSON []byte) []IdentifierPair {
	if len(eventJSON) == 0 {
		return nil
	}

	identifiers := make([]IdentifierPair, 0, 8) // pre-allocate for typical event size

	// Step 1: Extract userId.
	if userID := gjson.GetBytes(eventJSON, "userId"); userID.Exists() && userID.Type != gjson.Null {
		val := strings.TrimSpace(userID.String())
		if val != "" {
			identifiers = append(identifiers, IdentifierPair{Type: "user_id", Value: val})
		}
	}

	// Step 2: Extract anonymousId.
	if anonID := gjson.GetBytes(eventJSON, "anonymousId"); anonID.Exists() && anonID.Type != gjson.Null {
		val := strings.TrimSpace(anonID.String())
		if val != "" {
			identifiers = append(identifiers, IdentifierPair{Type: "anonymous_id", Value: val})
		}
	}

	// Step 3: Extract traits.email (common in identify events).
	// Check both traits.email and context.traits.email as fallback.
	emailExtracted := false
	if email := gjson.GetBytes(eventJSON, "traits.email"); email.Exists() && email.Type != gjson.Null {
		val := strings.TrimSpace(email.String())
		if val != "" {
			identifiers = append(identifiers, IdentifierPair{Type: "email", Value: val})
			emailExtracted = true
		}
	}
	if !emailExtracted {
		if email := gjson.GetBytes(eventJSON, "context.traits.email"); email.Exists() && email.Type != gjson.Null {
			val := strings.TrimSpace(email.String())
			if val != "" {
				identifiers = append(identifiers, IdentifierPair{Type: "email", Value: val})
			}
		}
	}

	// Step 4: Extract context.externalId array.
	// Each entry has format: {"type": "...", "id": "..."}
	// Field name is "context.externalId" (NOT "externalIds") per salesforce_bulk.go:84.
	externalIDs := gjson.GetBytes(eventJSON, "context.externalId")
	if externalIDs.Exists() && externalIDs.IsArray() {
		externalIDs.ForEach(func(_, entry gjson.Result) bool {
			idType := strings.TrimSpace(entry.Get("type").String())
			idValue := strings.TrimSpace(entry.Get("id").String())
			if idType != "" && idValue != "" {
				identifiers = append(identifiers, IdentifierPair{Type: idType, Value: idValue})
			}
			return true // continue iteration
		})
	}

	// Step 5: Validate and filter — remove empty pairs.
	valid := make([]IdentifierPair, 0, len(identifiers))
	for _, id := range identifiers {
		if !id.IsEmpty() {
			valid = append(valid, id)
		}
	}

	// Step 6: Deduplicate.
	return deduplicateExtractedIdentifiers(valid)
}

// FilterBlockedIdentifiers removes identifiers whose values are blocked by the
// resolution settings. This prevents blocked values (like "null", "0000", "-1")
// from corrupting the identity graph.
//
// Nil-safe: if settings is nil, returns all identifiers unchanged.
func FilterBlockedIdentifiers(identifiers []IdentifierPair, s *settings.ResolutionSettings) []IdentifierPair {
	if s == nil {
		return identifiers
	}
	result := make([]IdentifierPair, 0, len(identifiers))
	for _, id := range identifiers {
		if !s.IsBlocked(id.Type, id.Value) {
			result = append(result, id)
		}
	}
	return result
}

// SortByPriority sorts identifier pairs by their resolution priority.
// Higher priority identifiers (lower priority number) come first.
// This determines the order in which identifiers are used for resolution lookups.
//
// Uses stable sort to maintain relative order for same-priority identifiers.
// Nil-safe: no-op if settings is nil.
func SortByPriority(identifiers []IdentifierPair, s *settings.ResolutionSettings) {
	if s == nil {
		return
	}
	sort.SliceStable(identifiers, func(i, j int) bool {
		return s.CompareIdentifierPriority(identifiers[i].Type, identifiers[j].Type) < 0
	})
}

// deduplicateExtractedIdentifiers removes duplicate IdentifierPair entries.
// Two pairs are considered duplicates if they have the same Type AND Value.
// Preserves original order — first occurrence is kept.
func deduplicateExtractedIdentifiers(identifiers []IdentifierPair) []IdentifierPair {
	seen := make(map[string]struct{}, len(identifiers))
	result := make([]IdentifierPair, 0, len(identifiers))
	for _, id := range identifiers {
		key := id.Key()
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			result = append(result, id)
		}
	}
	return result
}
