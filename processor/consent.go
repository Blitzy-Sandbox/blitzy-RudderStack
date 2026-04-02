package processor

import (
	"fmt"

	"github.com/samber/lo"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/processor/enforcement"
	"github.com/rudderlabs/rudder-server/processor/types"
	"github.com/rudderlabs/rudder-server/utils/misc"
)

type ConsentManagementInfo struct {
	DeniedConsentIDs   []string `json:"deniedConsentIds"`
	AllowedConsentIDs  any      `json:"allowedConsentIds"` // Not used currently but added for future use
	Provider           string   `json:"provider"`
	ResolutionStrategy string   `json:"resolutionStrategy"`
}

type GenericConsentManagementProviderData struct {
	ResolutionStrategy string
	Consents           []string
}

type GenericConsentsConfig struct {
	Consent string `json:"consent"`
}

type GenericConsentManagementProviderConfig struct {
	Provider           string                  `json:"provider"`
	ResolutionStrategy string                  `json:"resolutionStrategy"`
	Consents           []GenericConsentsConfig `json:"consents"`
}

// ConsentViolation records a consent denial for a destination, used by Protocols
// enforcement integration (E-025). When enforcement modes are active alongside
// consent management, denied destinations produce ConsentViolation records that
// are used for structured logging, metrics emission, and downstream reporting.
type ConsentViolation struct {
	// DestinationID is the unique identifier of the destination that was denied.
	DestinationID string
	// Provider is the consent management provider that produced the denial
	// (e.g., "oneTrust", "ketch", "custom", or generic CMP provider names).
	Provider string
	// DeniedConsents lists the specific consent IDs that caused the denial.
	DeniedConsents []string
}

/*
Filters and returns destinations based on the consents configured for the destination and the user consents present in the event.

Supports legacy and generic consent management.
For GCM based filtering, uses source and destination IDs to fetch the appropriate GCM data from the config.
*/
func (proc *Handle) getConsentFilteredDestinations(event types.SingularEventT, sourceID string, destinations []backendconfig.DestinationT) []backendconfig.DestinationT {
	// If the event does not have denied consent IDs, do not filter any destinations
	consentManagementInfo, err := getConsentManagementInfo(event)
	if err != nil {
		// Log the error for debugging purposes
		proc.logger.Errorn("failed to get consent management info", obskit.Error(err))
	}

	if len(consentManagementInfo.DeniedConsentIDs) == 0 {
		return destinations
	}

	return lo.Filter(destinations, func(dest backendconfig.DestinationT, _ int) bool {
		// Generic consent management
		if cmpData := proc.getGCMData(sourceID, dest.ID, consentManagementInfo.Provider); len(cmpData.Consents) > 0 {

			finalResolutionStrategy := consentManagementInfo.ResolutionStrategy

			// For custom provider, the resolution strategy is to be picked from the destination config
			if consentManagementInfo.Provider == "custom" {
				finalResolutionStrategy = cmpData.ResolutionStrategy
			}

			switch finalResolutionStrategy {
			// The user must consent to at least one of the configured consents in the destination
			case "or":
				return !lo.Every(consentManagementInfo.DeniedConsentIDs, cmpData.Consents)

			// The user must consent to all of the configured consents in the destination
			default: // "and"
				return len(lo.Intersect(cmpData.Consents, consentManagementInfo.DeniedConsentIDs)) == 0
			}
		}

		// Legacy consent management
		if consentManagementInfo.Provider == "" || consentManagementInfo.Provider == "oneTrust" {
			// If the destination has oneTrustCookieCategories, returns false if any of the oneTrustCategories are present in deniedCategories
			if oneTrustCategories := proc.getOneTrustConsentData(dest.ID); len(oneTrustCategories) > 0 {
				return len(lo.Intersect(oneTrustCategories, consentManagementInfo.DeniedConsentIDs)) == 0
			}
		}

		if consentManagementInfo.Provider == "" || consentManagementInfo.Provider == "ketch" {
			// If the destination has ketchConsentPurposes, returns false if all ketchPurposes are present in deniedCategories
			if ketchPurposes := proc.getKetchConsentData(dest.ID); len(ketchPurposes) > 0 {
				return !lo.Every(consentManagementInfo.DeniedConsentIDs, ketchPurposes)
			}
		}

		return true
	})
}

// getConsentFilteredDestinationsWithEnforcement extends consent filtering with Protocols
// enforcement awareness (E-025). When an enforcement mode is provided (from tracking plan
// config), consent denials follow the enforcement mode:
//   - Block (or empty): reject the destination entirely — delegates to getConsentFilteredDestinations
//     for full backward compatibility. Returns nil violations.
//   - Omit: strip the destination from the list and collect ConsentViolation records for reporting.
//   - Allow: log consent violations but allow all destinations through — no filtering occurs.
//
// This method wraps getConsentFilteredDestinations and never modifies its behavior. The original
// method remains the source of truth for Block/empty mode filtering logic.
//
//nolint:unused // E-025: wired when enforcement mode is read from backend-config per-source
func (proc *Handle) getConsentFilteredDestinationsWithEnforcement(
	event types.SingularEventT,
	sourceID string,
	destinations []backendconfig.DestinationT,
	enforcementMode enforcement.Mode,
) ([]backendconfig.DestinationT, []ConsentViolation) {
	// For empty enforcement mode or Block mode, delegate to existing method (backward compatible).
	// No violation records are produced — this preserves the legacy code path exactly.
	if enforcementMode == "" || enforcementMode == enforcement.ModeBlock {
		return proc.getConsentFilteredDestinations(event, sourceID, destinations), nil
	}

	// Parse consent management info from the event
	consentManagementInfo, err := getConsentManagementInfo(event)
	if err != nil {
		proc.logger.Errorn("failed to get consent management info", obskit.Error(err))
	}

	// If no denied consent IDs exist in the event, all destinations pass through
	if len(consentManagementInfo.DeniedConsentIDs) == 0 {
		return destinations, nil
	}

	var violations []ConsentViolation
	filtered := make([]backendconfig.DestinationT, 0, len(destinations))

	for _, dest := range destinations {
		denied, provider, deniedConsents := proc.checkConsentDenied(dest, sourceID, consentManagementInfo)
		if denied {
			violations = append(violations, ConsentViolation{
				DestinationID:  dest.ID,
				Provider:       provider,
				DeniedConsents: deniedConsents,
			})

			switch enforcementMode {
			case enforcement.ModeAllow:
				// Allow mode: include the destination despite the consent denial.
				// The violation is recorded above for observability and Protocols reporting.
				filtered = append(filtered, dest)
			case enforcement.ModeOmit:
				// Omit mode: strip the destination (same as Block filtering behavior).
				// The violation is recorded above for reporting purposes.
			}
		} else {
			// Destination passes consent check — always included regardless of enforcement mode
			filtered = append(filtered, dest)
		}
	}

	// Log all collected violations for enforcement-mode-aware observability
	if len(violations) > 0 {
		proc.logConsentViolations(violations, sourceID, enforcementMode)
	}

	return filtered, violations
}

// checkConsentDenied evaluates whether a single destination would be denied by consent
// filtering and returns the denial details. This extracts the core consent check logic
// from getConsentFilteredDestinations for reuse by the enforcement-aware method.
//
// The check follows the same priority order as getConsentFilteredDestinations:
//  1. Generic Consent Management (GCM) — checked first for provider-specific data
//  2. Legacy OneTrust — checked if no GCM data and provider is "" or "oneTrust"
//  3. Legacy Ketch — checked if no previous match and provider is "" or "ketch"
//
// Returns denied=false if the destination has no consent configuration or passes all checks.
//
//nolint:unused // E-025: used by getConsentFilteredDestinationsWithEnforcement
func (proc *Handle) checkConsentDenied(
	dest backendconfig.DestinationT,
	sourceID string,
	info ConsentManagementInfo,
) (denied bool, provider string, deniedConsents []string) {
	// Generic consent management check — takes priority over legacy providers
	if cmpData := proc.getGCMData(sourceID, dest.ID, info.Provider); len(cmpData.Consents) > 0 {
		finalResolutionStrategy := info.ResolutionStrategy
		// For custom provider, the resolution strategy is picked from the destination config
		if info.Provider == "custom" {
			finalResolutionStrategy = cmpData.ResolutionStrategy
		}

		switch finalResolutionStrategy {
		case "or":
			// "or" resolution: user must consent to at least one configured consent.
			// lo.Every returns true when all consents are in the denied list → destination denied.
			if lo.Every(info.DeniedConsentIDs, cmpData.Consents) {
				return true, info.Provider, lo.Intersect(cmpData.Consents, info.DeniedConsentIDs)
			}
			return false, "", nil
		default: // "and"
			// "and" resolution: user must consent to all configured consents.
			// Any overlap between configured consents and denied IDs means destination denied.
			overlap := lo.Intersect(cmpData.Consents, info.DeniedConsentIDs)
			if len(overlap) > 0 {
				return true, info.Provider, overlap
			}
			return false, "", nil
		}
	}

	// Legacy OneTrust consent management
	if info.Provider == "" || info.Provider == "oneTrust" {
		if oneTrustCategories := proc.getOneTrustConsentData(dest.ID); len(oneTrustCategories) > 0 {
			overlap := lo.Intersect(oneTrustCategories, info.DeniedConsentIDs)
			if len(overlap) > 0 {
				return true, "oneTrust", overlap
			}
			return false, "", nil
		}
	}

	// Legacy Ketch consent management
	if info.Provider == "" || info.Provider == "ketch" {
		if ketchPurposes := proc.getKetchConsentData(dest.ID); len(ketchPurposes) > 0 {
			// lo.Every returns true when all purposes are in the denied list → destination denied
			if lo.Every(info.DeniedConsentIDs, ketchPurposes) {
				return true, "ketch", lo.Intersect(ketchPurposes, info.DeniedConsentIDs)
			}
			return false, "", nil
		}
	}

	// No consent configuration found for this destination — not denied
	return false, "", nil
}

// logConsentViolations logs consent violations for Protocols integration reporting (E-025).
// Each violation is logged as a structured info-level message with the source ID,
// destination ID, consent provider, and enforcement mode. This enables downstream
// monitoring, alerting, and audit trail capabilities for consent-enforcement integration.
//
//nolint:unused // E-025: called by getConsentFilteredDestinationsWithEnforcement
func (proc *Handle) logConsentViolations(violations []ConsentViolation, sourceID string, enforcementMode enforcement.Mode) {
	for _, v := range violations {
		proc.logger.Infon("consent violation detected",
			logger.NewStringField("sourceID", sourceID),
			logger.NewStringField("destinationID", v.DestinationID),
			logger.NewStringField("provider", v.Provider),
			logger.NewStringField("enforcementMode", string(enforcementMode)),
		)
	}
}

func (proc *Handle) getOneTrustConsentData(destinationID string) []string {
	proc.config.configSubscriberLock.RLock()
	defer proc.config.configSubscriberLock.RUnlock()
	return proc.config.oneTrustConsentCategoriesMap[destinationID]
}

func (proc *Handle) getKetchConsentData(destinationID string) []string {
	proc.config.configSubscriberLock.RLock()
	defer proc.config.configSubscriberLock.RUnlock()
	return proc.config.ketchConsentCategoriesMap[destinationID]
}

func (proc *Handle) getGCMData(sourceID, destinationID, provider string) GenericConsentManagementProviderData {
	proc.config.configSubscriberLock.RLock()
	defer proc.config.configSubscriberLock.RUnlock()

	defRetVal := GenericConsentManagementProviderData{}
	destinationData, ok := proc.config.genericConsentManagementMap[SourceID(sourceID)][DestinationID(destinationID)]
	if !ok {
		return defRetVal
	}

	providerData, ok := destinationData[ConsentProviderKey(provider)]
	if !ok {
		return defRetVal
	}

	return providerData
}

func getOneTrustConsentCategories(dest *backendconfig.DestinationT) []string {
	cookieCategories, ok := misc.MapLookup(dest.Config, "oneTrustCookieCategories").([]any)
	if !ok {
		// Handle the case where oneTrustCookieCategories is not a slice
		return nil
	}
	if len(cookieCategories) == 0 {
		return nil
	}
	return lo.FilterMap(cookieCategories, func(cookieCategory any, _ int) (string, bool) {
		switch category := cookieCategory.(type) {
		case map[string]any:
			cCategory, ok := category["oneTrustCookieCategory"].(string)
			return cCategory, ok && cCategory != ""
		default:
			return "", false
		}
	})
}

func getKetchConsentCategories(dest *backendconfig.DestinationT) []string {
	consentPurposes, ok := misc.MapLookup(dest.Config, "ketchConsentPurposes").([]any)
	if !ok {
		// Handle the case where ketchConsentPurposes is not a slice
		return nil
	}
	if len(consentPurposes) == 0 {
		return nil
	}
	return lo.FilterMap(consentPurposes, func(consentPurpose any, _ int) (string, bool) {
		switch t := consentPurpose.(type) {
		case map[string]any:
			purpose, ok := t["purpose"].(string)
			return purpose, ok && purpose != ""
		default:
			return "", false
		}
	})
}

func getGenericConsentManagementData(dest *backendconfig.DestinationT) (ConsentProviderMap, error) {
	genericConsentManagementData := make(ConsentProviderMap)

	if _, ok := dest.Config["consentManagement"]; !ok {
		return genericConsentManagementData, nil
	}

	consentManagementConfigBytes, mErr := jsonrs.Marshal(dest.Config["consentManagement"])
	if mErr != nil {
		return genericConsentManagementData, fmt.Errorf("error marshalling consentManagement: %v for destination ID: %s", mErr, dest.ID)
	}

	consentManagementConfig := make([]GenericConsentManagementProviderConfig, 0)
	unmErr := jsonrs.Unmarshal(consentManagementConfigBytes, &consentManagementConfig)

	if unmErr != nil {
		return genericConsentManagementData, fmt.Errorf("error unmarshalling consentManagementConfig: %v for destination ID: %s", unmErr, dest.ID)
	}

	for _, providerConfig := range consentManagementConfig {
		consentsConfig := providerConfig.Consents

		if len(consentsConfig) > 0 && providerConfig.Provider != "" {
			consentIDs := lo.FilterMap(
				consentsConfig,
				func(consentsObj GenericConsentsConfig, _ int) (string, bool) {
					return consentsObj.Consent, consentsObj.Consent != ""
				},
			)

			if len(consentIDs) > 0 {
				genericConsentManagementData[ConsentProviderKey(providerConfig.Provider)] = GenericConsentManagementProviderData{
					ResolutionStrategy: providerConfig.ResolutionStrategy,
					Consents:           consentIDs,
				}
			}
		}
	}

	return genericConsentManagementData, nil
}

func getConsentManagementInfo(event types.SingularEventT) (ConsentManagementInfo, error) {
	consentManagementInfo := ConsentManagementInfo{}
	if consentManagement, ok := misc.MapLookup(event, "context", "consentManagement").(map[string]any); ok {
		consentManagementObjBytes, mErr := jsonrs.Marshal(consentManagement)
		if mErr != nil {
			return consentManagementInfo, fmt.Errorf("error marshalling consentManagement: %v", mErr)
		}

		unmErr := jsonrs.Unmarshal(consentManagementObjBytes, &consentManagementInfo)
		if unmErr != nil {
			return consentManagementInfo, fmt.Errorf("error unmarshalling consentManagementInfo: %v", unmErr)
		}

		filterPredicate := func(consent string, _ int) (string, bool) {
			return consent, consent != ""
		}

		consentManagementInfo.DeniedConsentIDs = lo.FilterMap(consentManagementInfo.DeniedConsentIDs, filterPredicate)
	}

	return consentManagementInfo, nil
}
