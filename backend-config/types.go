package backendconfig

import (
	"encoding/json"
	"time"

	"github.com/samber/lo"

	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/backend-config/dynamicconfig"
)

// Topic refers to a subset of backend config's updates, received after subscribing using the backend config's Subscribe function.
type Topic string

type Regulation string

const (
	/*TopicBackendConfig topic provides updates on full backend config, via Subscribe function */
	TopicBackendConfig Topic = "backendConfig"

	/*TopicProcessConfig topic provides updates on backend config of processor enabled destinations, via Subscribe function */
	TopicProcessConfig Topic = "processConfig"

	/*TopicFunctionsConfig topic provides updates on backend config relevant to Functions runtime, via Subscribe function */
	TopicFunctionsConfig Topic = "functionsConfig"

	/*TopicProtocolsConfig topic provides updates on backend config relevant to Protocols enforcement, via Subscribe function */
	TopicProtocolsConfig Topic = "protocolsConfig"

	/*TopicIdentityConfig topic provides updates on backend config relevant to Identity resolution, via Subscribe function */
	TopicIdentityConfig Topic = "identityConfig"

	/*RegulationSuppress refers to Suppress Regulation */
	RegulationSuppress Regulation = "Suppress"

	/*RegulationDelete refers to Suppress and Delete Regulation */
	RegulationDelete Regulation = "Delete" // TODO Will add support soon.

	/*RegulationSuppressAndDelete refers to Suppress and Delete Regulation */
	RegulationSuppressAndDelete Regulation = "Suppress_With_Delete"

	GlobalEventType = "global"
)

// EnforcementMode defines the tracking plan enforcement behavior.
// Block: Block the entire event from proceeding
// Omit: Omit violating properties but allow the event through
// Allow: Allow the event through and log the violation
type EnforcementMode string

const (
	EnforcementModeBlock EnforcementMode = "block"
	EnforcementModeOmit  EnforcementMode = "omit"
	EnforcementModeAllow EnforcementMode = "allow"
)

// TrackingPlanEnforcementConfig defines enforcement settings for a tracking plan.
// It supports configurable enforcement per event call type (track, identify, group, page, screen).
type TrackingPlanEnforcementConfig struct {
	// Global is the default enforcement mode applied to all event types
	Global EnforcementMode `json:"global"`
	// PerCallType overrides enforcement mode for specific call types (e.g., "track" -> "block")
	PerCallType map[string]EnforcementMode `json:"perCallType,omitempty"`
	// ForwardBlockedEventsSourceID is the source ID to forward blocked events to (E-023)
	ForwardBlockedEventsSourceID string `json:"forwardBlockedEventsSourceId,omitempty"`
}

// BlockedValue defines a pattern for blocking identity merge on specific values.
// Prevents merge-all scenarios that could corrupt the identity graph.
type BlockedValue struct {
	// MatchType specifies how to match: "exact" or "regex"
	MatchType string `json:"matchType"`
	// Value is the exact string or regex pattern to block
	Value string `json:"value"`
	// IdentifierType optionally restricts this rule to a specific identifier type (e.g., "email")
	IdentifierType string `json:"identifierType,omitempty"`
}

// IdentifierLimit defines a limit on how many identifiers of a given type can be associated per profile.
type IdentifierLimit struct {
	// Period specifies the time window: "weekly", "monthly", "annually", "ever"
	Period string `json:"period"`
	// MaxCount is the maximum number of identifiers allowed in the period
	MaxCount int `json:"maxCount"`
}

// IdentityResolutionSettings configures the identity resolution behavior for a workspace or source.
type IdentityResolutionSettings struct {
	// Enabled toggles identity resolution on/off
	Enabled bool `json:"enabled"`
	// BlockedValues lists patterns to block during identity merge to prevent merge-all
	BlockedValues []BlockedValue `json:"blockedValues,omitempty"`
	// IdentifierLimits maps identifier type (e.g., "email", "user_id") to its limit
	IdentifierLimits map[string]IdentifierLimit `json:"identifierLimits,omitempty"`
	// IdentifierPriority is an ordered list of identifier types for resolution priority
	IdentifierPriority []string `json:"identifierPriority,omitempty"`
}

// FunctionBinding represents an association between a destination/source and a function.
type FunctionBinding struct {
	// FunctionID is the unique identifier of the bound function
	FunctionID string `json:"functionId"`
	// FunctionType indicates the type: "source", "destination", or "insert"
	FunctionType string `json:"functionType"`
	// Enabled toggles the function binding on/off
	Enabled bool `json:"enabled"`
}

// TrackingPlanConfigT represents a full tracking plan configuration for management API integration.
type TrackingPlanConfigT struct {
	ID                string                        `json:"id"`
	Name              string                        `json:"name"`
	Version           int                           `json:"version"`
	Schema            json.RawMessage               `json:"schema"`
	EnforcementConfig TrackingPlanEnforcementConfig `json:"enforcementConfig"`
	CreatedAt         time.Time                     `json:"createdAt"`
	UpdatedAt         time.Time                     `json:"updatedAt"`
}

type DestinationDefinitionT struct {
	ID          string
	Name        string
	DisplayName string
	Config      map[string]any
}

type SourceDefinitionT struct {
	ID       string
	Name     string
	Category string
	Type     string // // Indicates whether source is one of {cloud, web, flutter, android, ios, warehouse, cordova, amp, reactnative, unity}. This field is not present in sources table
	Options  SourceDefinitionOptions
}

type SourceDefinitionOptions struct {
	Hydration struct {
		Enabled bool
	}
}

type DestinationT struct {
	ID                    string
	Name                  string
	DestinationDefinition DestinationDefinitionT
	Config                map[string]any
	Enabled               bool
	WorkspaceID           string
	Transformations       []TransformationT
	IsProcessorEnabled    bool
	RevisionID            string
	DeliveryAccount       *Account          `json:"deliveryAccount,omitempty"`
	DeleteAccount         *Account          `json:"deleteAccount,omitempty"`
	HasDynamicConfig      bool              `json:"hasDynamicConfig"`
	FunctionBindings      []FunctionBinding `json:"functionBindings,omitempty"`
}

// UpdateHasDynamicConfig checks if the destination config contains dynamic config patterns
// and sets the HasDynamicConfig field accordingly.
// It uses a cache to avoid recomputing the flag for destinations that haven't changed.
// The cache is keyed by destination ID and stores the RevisionID and HasDynamicConfig values.
// When a destination's RevisionID changes, it indicates a config change, and we recompute the flag.
func (d *DestinationT) UpdateHasDynamicConfig(cache dynamicconfig.Cache) {
	// Check if we have a cached value for this destination
	cachedInfo, exists := cache.Get(d.ID)

	// If the destination's RevisionID matches the cached RevisionID,
	// use the cached HasDynamicConfig value to avoid recomputation
	if exists && d.RevisionID == cachedInfo.RevisionID {
		d.HasDynamicConfig = cachedInfo.HasDynamicConfig
		return
	}

	// RevisionID is not in cache or has changed, recompute the dynamic config flag
	d.HasDynamicConfig = dynamicconfig.ContainsPattern(d.Config)

	pkgLogger.Infon("HasDynamicConfig flag updated",
		obskit.DestinationID(d.ID),
		obskit.WorkspaceID(d.WorkspaceID),
		logger.NewBoolField("hasDynamicConfig", d.HasDynamicConfig),
	)

	// Update the cache with the new value
	cache.Set(d.ID, &dynamicconfig.DestinationRevisionInfo{
		RevisionID:       d.RevisionID,
		HasDynamicConfig: d.HasDynamicConfig,
	})
}

type SourceT struct {
	ID                         string
	OriginalID                 string
	Name                       string
	SourceDefinition           SourceDefinitionT
	Config                     json.RawMessage
	Enabled                    bool
	WorkspaceID                string
	Destinations               []DestinationT
	WriteKey                   string
	DgSourceTrackingPlanConfig DgSourceTrackingPlanConfigT
	Transient                  bool
	GeoEnrichment              struct {
		Enabled bool
	}
	InternalSecret     json.RawMessage
	IdentityResolution IdentityResolutionSettings `json:"identityResolution,omitempty"`
	SourceFunctionID   string                     `json:"sourceFunctionId,omitempty"`
}

type Credential struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	IsSecret bool   `json:"isSecret"`
}

func (s *SourceT) IsReplaySource() bool {
	return s.OriginalID != ""
}

func (s *SourceT) IsSourceHydrationSupported() bool {
	return s.SourceDefinition.Options.Hydration.Enabled
}

type Account struct {
	ID                    string             `json:"id"`
	AccountDefinitionName string             `json:"accountDefinitionName"`
	Options               map[string]any     `json:"options"`
	Secret                map[string]any     `json:"secret"`
	AccountDefinition     *AccountDefinition `json:"accountDefinition"`
}

type AccountDefinition struct {
	Name               string         `json:"name"`
	Config             map[string]any `json:"config"`
	AuthenticationType string         `json:"authenticationType"`
}
type ConfigT struct {
	EnableMetrics      bool                         `json:"enableMetrics"`
	WorkspaceID        string                       `json:"workspaceId"`
	Sources            []SourceT                    `json:"sources"`
	EventReplays       map[string]EventReplayConfig `json:"eventReplays"`
	Libraries          LibrariesT                   `json:"libraries"`
	ConnectionFlags    ConnectionFlags              `json:"flags"`
	Settings           Settings                     `json:"settings"`
	UpdatedAt          time.Time                    `json:"updatedAt"`
	Credentials        map[string]Credential        `json:"credentials"`
	Connections        map[string]Connection        `json:"connections"`
	Accounts           map[string]Account           `json:"accounts"`
	AccountDefinitions map[string]AccountDefinition `json:"accountDefinitions"`
	TrackingPlans      []TrackingPlanConfigT        `json:"trackingPlans,omitempty"`
}

type Connection struct {
	SourceID         string         `json:"sourceId"`
	DestinationID    string         `json:"destinationId"`
	Enabled          bool           `json:"enabled"`
	Config           map[string]any `json:"config"`
	ProcessorEnabled bool           `json:"processorEnabled"`
}

func (c *ConfigT) SourcesMap() map[string]*SourceT {
	sourcesMap := make(map[string]*SourceT)
	for i := range c.Sources {
		source := c.Sources[i]
		sourcesMap[source.ID] = &source
	}
	return sourcesMap
}

func (c *ConfigT) DestinationsMap() map[string]*DestinationT {
	destinationsMap := make(map[string]*DestinationT)
	for i := range c.Sources {
		source := c.Sources[i]
		for j := range source.Destinations {
			destination := source.Destinations[j]
			destinationsMap[destination.ID] = &destination
		}
	}
	return destinationsMap
}

type Settings struct {
	DataRetention      DataRetention              `json:"dataRetention"`
	EventAuditEnabled  bool                       `json:"eventAuditEnabled"`
	EventBlocking      EventBlocking              `json:"eventBlocking"`
	IdentityResolution IdentityResolutionSettings `json:"identityResolution,omitempty"`
}

type DataRetention struct {
	DisableReportingPII bool               `json:"disableReportingPii"`
	UseSelfStorage      bool               `json:"useSelfStorage"`
	StorageBucket       StorageBucket      `json:"storageBucket"`
	StoragePreferences  StoragePreferences `json:"storagePreferences"`
	RetentionPeriod     string             `json:"retentionPeriod"`
}

type StorageBucket struct {
	Type   string `json:"type"`
	Config map[string]any
}

type StoragePreferences struct {
	GatewayDumps bool `json:"gatewayDumps"`
}

func (sp StoragePreferences) Backup(tableprefix string) bool {
	switch tableprefix {
	case "gw":
		return sp.GatewayDumps
	default:
		return false
	}
}

type ConnectionFlags struct {
	URL      string          `json:"url"`
	Services map[string]bool `json:"services"`
}

type TransformationT struct {
	VersionID string
	ID        string
	Config    map[string]any
	Language  string
}

type LibraryT struct {
	VersionID string
}

type LibrariesT []LibraryT

type DgSourceTrackingPlanConfigT struct {
	SourceId            string                        `json:"sourceId"`
	SourceConfigVersion int                           `json:"version"`
	Config              map[string]map[string]any     `json:"config"`
	MergedConfig        map[string]any                `json:"mergedConfig"`
	Deleted             bool                          `json:"deleted"`
	TrackingPlan        TrackingPlanT                 `json:"trackingPlan"`
	EnforcementConfig   TrackingPlanEnforcementConfig `json:"enforcementConfig,omitempty"`
}

func (dgSourceTPConfigT *DgSourceTrackingPlanConfigT) GetMergedConfig(eventType string) map[string]any {
	if dgSourceTPConfigT.MergedConfig == nil {
		globalConfig := dgSourceTPConfigT.fetchEventConfig(GlobalEventType)
		eventSpecificConfig := dgSourceTPConfigT.fetchEventConfig(eventType)
		outputConfig := lo.Assign(globalConfig, eventSpecificConfig)
		// Gap 9 (E-020): Propagate schemaVersion from TrackingPlanT into MergedConfig
		// so that processor/trackingplan.go shouldUseLocalValidation() can detect
		// "draft-07" and activate local JSON Schema validation. Without this, the
		// schemaVersion is only present in the TrackingPlan struct but never in
		// MergedTpConfig, causing local validation to never activate.
		if dgSourceTPConfigT.TrackingPlan.SchemaVersion != "" {
			if _, exists := outputConfig["schemaVersion"]; !exists {
				outputConfig["schemaVersion"] = dgSourceTPConfigT.TrackingPlan.SchemaVersion
			}
		}
		dgSourceTPConfigT.MergedConfig = outputConfig
	}
	return dgSourceTPConfigT.MergedConfig
}

func (dgSourceTPConfigT *DgSourceTrackingPlanConfigT) fetchEventConfig(eventType string) map[string]any {
	emptyMap := map[string]any{}
	_, eventSpecificConfigPresent := dgSourceTPConfigT.Config[eventType]
	if !eventSpecificConfigPresent {
		return emptyMap
	}
	return dgSourceTPConfigT.Config[eventType]
}

type TrackingPlanT struct {
	Id            string `json:"id"`
	Version       int    `json:"version"`
	Name          string `json:"name,omitempty"`
	SchemaVersion string `json:"schemaVersion,omitempty"` // JSON Schema draft version, e.g., "draft-07"
}

type EventBlocking struct {
	Events map[string][]string `json:"events"`
}
