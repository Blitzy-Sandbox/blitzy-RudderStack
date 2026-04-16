package processor

import (
	"context"
	"strconv"
	"time"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"

	backendconfig "github.com/rudderlabs/rudder-server/backend-config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/processor/enforcement"
	"github.com/rudderlabs/rudder-server/processor/types"
	schema "github.com/rudderlabs/rudder-server/protocols/schema"
	reportingtypes "github.com/rudderlabs/rudder-server/utils/types"
)

type TrackingPlanStatT struct {
	numEvents                   stats.Measurement
	numValidationSuccessEvents  stats.Measurement
	numValidationFailedEvents   stats.Measurement
	numValidationFilteredEvents stats.Measurement
	tpValidationTime            stats.Measurement
	numBlockedEvents            stats.Measurement // Events blocked by Block enforcement mode (E-022)
	numOmittedProps             stats.Measurement // Properties omitted by Omit enforcement mode (E-022)
	numAllowedViolations        stats.Measurement // Violations allowed by Allow enforcement mode (E-022)
}

// reportViolations adds violation information to event context based on enforcement mode.
// Modes: Block (reject event), Omit (strip properties), Allow (log + pass through).
// Falls back to legacy propagateValidationErrors behavior when enforcement mode is not set,
// ensuring full backward compatibility with existing pipeline behavior (Rule 0.7.6).
func reportViolations(validateEvent *types.TransformerResponse, trackingPlanID string, trackingPlanVersion int, enforcementMode enforcement.Mode) {
	// Backward compatibility: when enforcement mode is not set (zero value),
	// use the legacy binary propagateValidationErrors toggle.
	if enforcementMode == "" {
		if validateEvent.Metadata.MergedTpConfig["propagateValidationErrors"] == "false" {
			return
		}
	}

	validationErrors := validateEvent.ValidationErrors
	output := validateEvent.Output

	// Ensure context map exists in the output for violation enrichment
	eventContext, ok := output["context"]
	if !ok || eventContext == nil {
		ctx := make(map[string]any)
		ctx["trackingPlanId"] = trackingPlanID
		ctx["trackingPlanVersion"] = trackingPlanVersion
		ctx["violationErrors"] = validationErrors
		output["context"] = ctx
	} else {
		ctx, castOk := eventContext.(map[string]any)
		if !castOk {
			return
		}
		ctx["trackingPlanId"] = trackingPlanID
		ctx["trackingPlanVersion"] = trackingPlanVersion
		ctx["violationErrors"] = validationErrors
	}

	// Apply enforcement mode-specific actions (E-022)
	switch enforcementMode {
	case enforcement.ModeBlock:
		// Mark the event for blocking — downstream pipeline stages will reject it.
		// The event is preserved with violation context for debugging and optional forwarding.
		if ctx, ok := output["context"].(map[string]any); ok {
			ctx["blocked"] = true
		}
	case enforcement.ModeOmit:
		// Strip violating properties from event output while preserving conforming data.
		// Omitted property names are recorded in context for observability.
		stripViolatingProperties(validateEvent)
	case enforcement.ModeAllow:
		// Allow mode: violation info already added to context above, event passes through unchanged.
		// This is equivalent to the legacy propagateValidationErrors=true behavior.
	}
}

// stripViolatingProperties removes properties from the event output that violated the
// tracking plan schema. Property names are extracted from the event's ValidationErrors.
// The names of omitted properties are added to the event context for observability.
func stripViolatingProperties(validateEvent *types.TransformerResponse) {
	if len(validateEvent.ValidationErrors) == 0 {
		return
	}
	output := validateEvent.Output
	propertiesRaw, ok := output["properties"]
	if !ok || propertiesRaw == nil {
		return
	}
	props, castOk := propertiesRaw.(map[string]any)
	if !castOk {
		return
	}
	// Remove each property that appears in the validation errors
	var omittedProperties []string
	for _, ve := range validateEvent.ValidationErrors {
		if ve.Property != "" {
			if _, exists := props[ve.Property]; exists {
				delete(props, ve.Property)
				omittedProperties = append(omittedProperties, ve.Property)
			}
		}
	}
	// Record omitted property names in event context for downstream observability
	if len(omittedProperties) > 0 {
		if ctx, ok := output["context"].(map[string]any); ok {
			ctx["omittedProperties"] = omittedProperties
		}
	}
}

// enhanceWithViolation enhances ValidationErrors in the event context for both
// successful and failed events based on the specified enforcement mode.
// When enforcementMode is empty, legacy propagateValidationErrors behavior applies.
func enhanceWithViolation(response types.Response, trackingPlanID string, trackingPlanVersion int, enforcementMode enforcement.Mode) {
	for i := range response.Events {
		validatedEvent := &response.Events[i]
		reportViolations(validatedEvent, trackingPlanID, trackingPlanVersion, enforcementMode)
	}

	for i := range response.FailedEvents {
		validatedEvent := &response.FailedEvents[i]
		reportViolations(validatedEvent, trackingPlanID, trackingPlanVersion, enforcementMode)
	}
}

// validateEvents validates events against tracking plans. If a TrackingPlanId exists for a
// source's events, they are validated via the transformer (or locally for draft-07 schemas).
// The response contains both Events (passed) and FailedEvents (violations).
//
// Enhanced for E-020 (JSON Schema draft-07), E-021 (anomaly detection hooks),
// E-022 (three enforcement modes), and E-023 (forward-blocked-events routing).
func (proc *Handle) validateEvents(groupedEventsBySourceId map[SourceIDT][]types.TransformerEvent, eventsByMessageID map[string]types.SingularEventWithReceivedAt, srcHydrationEnabledMap map[SourceIDT]bool) (map[SourceIDT][]types.TransformerEvent, []*reportingtypes.PUReportedMetric, sourceIDPipelineSteps) {
	validatedEventsBySourceId := make(map[SourceIDT][]types.TransformerEvent)
	validatedReportMetrics := make([]*reportingtypes.PUReportedMetric, 0)
	sourcePipelineSteps := make(sourceIDPipelineSteps)
	for enabledSourceId, enabled := range srcHydrationEnabledMap {
		sourcePipelineSteps[enabledSourceId] = SourcePipelineSteps{srcHydration: enabled}
	}

	for sourceId := range groupedEventsBySourceId {
		eventList := groupedEventsBySourceId[sourceId]

		trackingPlanID := eventList[0].Metadata.TrackingPlanID
		trackingPlanVersion := eventList[0].Metadata.TrackingPlanVersion

		if trackingPlanID == "" {
			// pass on the jobs for transformation(User, Dest)
			validatedEventsBySourceId[sourceId] = append(validatedEventsBySourceId[sourceId], eventList...)
			continue
		}

		// Resolve enforcement mode from tracking plan config (E-022).
		// Supports per-source and per-call-type overrides. Returns empty Mode when not configured,
		// which triggers backward-compatible legacy propagateValidationErrors behavior.
		enforcementMode := enforcement.ResolveModeFromConfig(
			eventList[0].Metadata.MergedTpConfig,
			eventList[0].Metadata.EventType,
		)

		validationStat := proc.newValidationStat(&eventList[0].Metadata)
		validationStat.numEvents.Count(len(eventList))
		transformerEvent := eventList[0]

		commonMetaData := transformerEvent.Metadata.CommonMetadata()

		// Validation: use local JSON Schema draft-07 validator when configured (E-020),
		// otherwise fall back to Transformer-delegated validation (backward compatible).
		validationStart := time.Now()
		var response types.Response
		if proc.shouldUseLocalValidation(eventList) {
			response = proc.validateEventsLocally(eventList)
		} else {
			response = proc.transformerClients.TrackingPlan().Validate(context.TODO(), eventList)
		}
		validationStat.tpValidationTime.Since(validationStart)

		// Safety check: if transformerInput count does not match transformerOutput count,
		// discard the validation output and pass events through unvalidated.
		if (len(response.Events) + len(response.FailedEvents)) != len(eventList) {
			validatedEventsBySourceId[sourceId] = append(validatedEventsBySourceId[sourceId], eventList...)
			continue
		}

		// Enrich events with violation information based on the resolved enforcement mode
		enhanceWithViolation(response, trackingPlanID, trackingPlanVersion, enforcementMode)

		// Anomaly detection: observe events for unexpected names/properties not in tracking plan (E-021)
		if proc.anomalyDetector != nil {
			proc.anomalyDetector.Observe(string(sourceId), eventList, response)
		}

		// Set sourcePipelineSteps.trackingPlanValidation for the sourceID to true.
		// This is being used to distinguish the flows in reporting service
		sourceSteps := sourcePipelineSteps[sourceId]
		sourceSteps.trackingPlanValidation = true
		sourcePipelineSteps[sourceId] = sourceSteps

		inPU := reportingtypes.DESTINATION_FILTER
		if sourcePipelineSteps[sourceId].srcHydration {
			inPU = reportingtypes.SOURCE_HYDRATION
		}

		var successMetrics []*reportingtypes.PUReportedMetric
		eventsToTransform, successMetrics, _, _ := proc.getTransformerEvents(response, commonMetaData, eventsByMessageID, &transformerEvent.Destination, backendconfig.Connection{}, inPU, reportingtypes.TRACKINGPLAN_VALIDATOR) // Note: Sending false for usertransformation enabled is safe because this stage is before user transformation.
		nonSuccessMetrics := proc.getNonSuccessfulMetrics(response, eventList, commonMetaData, eventsByMessageID, inPU, reportingtypes.TRACKINGPLAN_VALIDATOR)

		validationStat.numValidationSuccessEvents.Count(len(eventsToTransform))
		validationStat.numValidationFailedEvents.Count(len(nonSuccessMetrics.failedJobs))
		validationStat.numValidationFilteredEvents.Count(len(nonSuccessMetrics.filteredJobs))

		// Record enforcement mode-specific metrics (E-022)
		switch enforcementMode {
		case enforcement.ModeBlock:
			validationStat.numBlockedEvents.Count(len(nonSuccessMetrics.failedJobs))
		case enforcement.ModeOmit:
			validationStat.numOmittedProps.Count(countOmittedProperties(response))
		case enforcement.ModeAllow:
			validationStat.numAllowedViolations.Count(len(nonSuccessMetrics.failedJobs) + countViolationsInSuccessEvents(response))
		}

		proc.logger.Debugn("Validation output size",
			logger.NewIntField("outputSize", int64(len(eventsToTransform))),
			logger.NewStringField("enforcementMode", string(enforcementMode)),
		)

		// Forward blocked events to alternative source if configured (E-023).
		// Only applies when enforcement mode is Block and there are failed events.
		if enforcementMode == enforcement.ModeBlock && len(nonSuccessMetrics.failedJobs) > 0 {
			if forwardSourceID := enforcement.GetForwardSourceID(eventList[0].Metadata.MergedTpConfig); forwardSourceID != "" {
				proc.forwardBlockedEvents(nonSuccessMetrics.failedJobs, forwardSourceID)
			}
		}

		// REPORTING - START
		if proc.isReportingEnabled() {
			// There will be no diff metrics for tracking plan validation
			validatedReportMetrics = append(validatedReportMetrics, successMetrics...)
			validatedReportMetrics = append(validatedReportMetrics, nonSuccessMetrics.failedMetrics...)
			validatedReportMetrics = append(validatedReportMetrics, nonSuccessMetrics.filteredMetrics...)
		}
		// REPORTING - END

		// Gap 3 (E-022): Enforce Block mode by filtering out events marked as blocked.
		// After enhanceWithViolation sets context["blocked"] = true on events that violate
		// the tracking plan under Block enforcement mode, we must remove them from the
		// pipeline to prevent them from reaching the transformer and router.
		if enforcementMode == enforcement.ModeBlock {
			var unblockedEvents []types.TransformerEvent
			for _, ev := range eventsToTransform {
				if ctx, ok := ev.Message["context"].(map[string]any); ok {
					if blocked, _ := ctx["blocked"].(bool); blocked {
						continue // drop blocked event from pipeline
					}
				}
				unblockedEvents = append(unblockedEvents, ev)
			}
			eventsToTransform = unblockedEvents
		}

		if len(eventsToTransform) == 0 {
			continue
		}
		validatedEventsBySourceId[sourceId] = append(validatedEventsBySourceId[sourceId], eventsToTransform...)
	}
	return validatedEventsBySourceId, validatedReportMetrics, sourcePipelineSteps
}

// shouldUseLocalValidation checks if local JSON Schema draft-07 validation should be used
// based on the tracking plan configuration. When "schemaVersion" is set to "draft-07" in
// the MergedTpConfig, local validation is preferred over Transformer delegation.
// Falls back to Transformer when not configured, maintaining backward compatibility.
func (proc *Handle) shouldUseLocalValidation(events []types.TransformerEvent) bool {
	if len(events) == 0 {
		return false
	}
	schemaVersion, ok := events[0].Metadata.MergedTpConfig["schemaVersion"]
	if !ok {
		return false
	}
	sv, isString := schemaVersion.(string)
	if !isString {
		return false
	}
	return sv == "draft-07"
}

// validateEventsLocally performs local JSON Schema draft-07 validation without
// calling the external Transformer service (E-020). Uses the protocols/schema
// package backed by santhosh-tekuri/jsonschema/v5 for full draft-07 support:
// required fields, regex patterns, nested objects, enum values, full type enforcement.
//
// Schema extraction: the "schema" key in MergedTpConfig holds the JSON Schema
// definition for the event type. This is populated by backend-config via
// DgSourceTrackingPlanConfigT.GetMergedConfig(). If the schema is absent or
// invalid, the method falls back to the Transformer for backward compatibility.
//
// Response format matches the Transformer contract exactly:
//   - Events with no violations → types.TransformerResponse{StatusCode: 200, Output: event.Message}
//   - Events with violations → types.TransformerResponse{StatusCode: 400, ValidationErrors: [...]}
func (proc *Handle) validateEventsLocally(events []types.TransformerEvent) types.Response {
	if len(events) == 0 {
		return types.Response{}
	}

	// Extract the JSON Schema from MergedTpConfig. The "schema" key holds the
	// tracking plan's JSON Schema definition for this event type.
	mergedTpConfig := events[0].Metadata.MergedTpConfig
	schemaRaw, hasSchema := mergedTpConfig["schema"]
	if !hasSchema {
		// No schema in config — fall back to Transformer for backward compatibility.
		proc.logger.Debugn("No schema in MergedTpConfig, falling back to Transformer")
		return proc.transformerClients.TrackingPlan().Validate(context.TODO(), events)
	}

	// Serialize the schema to JSON bytes for the validator.
	schemaBytes, marshalErr := jsonrs.Marshal(schemaRaw)
	if marshalErr != nil {
		proc.logger.Warnn("Failed to marshal tracking plan schema, falling back to Transformer",
			logger.NewStringField("error", marshalErr.Error()),
		)
		return proc.transformerClients.TrackingPlan().Validate(context.TODO(), events)
	}

	// Compile the JSON Schema once for the entire batch.
	compiled, compileErr := schema.CompileSchema(schemaBytes)
	if compileErr != nil {
		proc.logger.Warnn("Failed to compile tracking plan schema, falling back to Transformer",
			logger.NewStringField("error", compileErr.Error()),
		)
		return proc.transformerClients.TrackingPlan().Validate(context.TODO(), events)
	}

	// Validate each event and build the response matching Transformer output format.
	// SingularEventT is already map[string]any — no type assertion needed.
	var response types.Response
	for _, event := range events {
		eventMessage := map[string]any(event.Message)

		validationErrors, validateErr := schema.ValidateWithCompiled(compiled, eventMessage)
		if validateErr != nil {
			// Internal validation error — pass event through as success to avoid
			// blocking the pipeline on infrastructure failures.
			proc.logger.Warnn("Schema validation internal error, passing event through",
				logger.NewStringField("error", validateErr.Error()),
				logger.NewStringField("messageId", event.Metadata.MessageID),
			)
			response.Events = append(response.Events, types.TransformerResponse{
				Output:     eventMessage,
				Metadata:   event.Metadata,
				StatusCode: 200,
			})
			continue
		}

		if len(validationErrors) == 0 {
			// Event passes validation — add to success events.
			response.Events = append(response.Events, types.TransformerResponse{
				Output:     eventMessage,
				Metadata:   event.Metadata,
				StatusCode: 200,
			})
		} else {
			// Event has validation violations — convert to processor format.
			procErrors := make([]types.ValidationError, len(validationErrors))
			for i, ve := range validationErrors {
				procErrors[i] = types.ValidationError{
					Type:     ve.Constraint,
					Message:  ve.Message,
					Property: ve.FieldPath,
					Meta: map[string]string{
						"expectedType": ve.ExpectedType,
						"actualValue":  ve.ActualValue,
					},
				}
			}
			response.FailedEvents = append(response.FailedEvents, types.TransformerResponse{
				Output:           eventMessage,
				Metadata:         event.Metadata,
				StatusCode:       400,
				Error:            "event validation failed against tracking plan schema",
				ValidationErrors: procErrors,
			})
		}
	}

	return response
}

// forwardBlockedEvents routes blocked events to an alternative source for debugging (E-023).
// Events are preserved with their original metadata and forwarded via the enforcement forwarder.
// When the forwarder is nil, blocked events are simply dropped (not forwarded).
func (proc *Handle) forwardBlockedEvents(failedJobs []*jobsdb.JobT, forwardSourceID string) {
	if proc.enforcementForwarder != nil {
		proc.enforcementForwarder.Forward(failedJobs, forwardSourceID)
	}
}

// countOmittedProperties counts the total number of properties that were omitted across
// all successfully validated events in the response. Used for Omit mode metrics.
func countOmittedProperties(response types.Response) int {
	count := 0
	for _, event := range response.Events {
		if ctx, ok := event.Output["context"].(map[string]any); ok {
			if omitted, ok := ctx["omittedProperties"].([]string); ok {
				count += len(omitted)
			}
		}
	}
	return count
}

// countViolationsInSuccessEvents counts validation errors across events that passed
// validation (success events). Used for Allow mode metrics where events with violations
// are still passed through the pipeline.
func countViolationsInSuccessEvents(response types.Response) int {
	count := 0
	for _, event := range response.Events {
		count += len(event.ValidationErrors)
	}
	return count
}

// newValidationStat creates a new TrackingPlanStatT instance with tagged metrics
// for tracking plan validation reporting, including enforcement mode metrics (E-022).
func (proc *Handle) newValidationStat(metadata *types.Metadata) *TrackingPlanStatT {
	tags := map[string]string{
		"destination":         metadata.DestinationID,
		"destType":            metadata.DestinationType,
		"source":              metadata.SourceID,
		"workspaceId":         metadata.WorkspaceID,
		"trackingPlanId":      metadata.TrackingPlanID,
		"trackingPlanVersion": strconv.Itoa(metadata.TrackingPlanVersion),
	}

	numEvents := proc.statsFactory.NewTaggedStat("proc_num_tp_input_events", stats.CountType, tags)
	numValidationSuccessEvents := proc.statsFactory.NewTaggedStat("proc_num_tp_output_success_events", stats.CountType, tags)
	numValidationFailedEvents := proc.statsFactory.NewTaggedStat("proc_num_tp_output_failed_events", stats.CountType, tags)
	numValidationFilteredEvents := proc.statsFactory.NewTaggedStat("proc_num_tp_output_filtered_events", stats.CountType, tags)
	tpValidationTime := proc.statsFactory.NewTaggedStat("proc_tp_validation", stats.TimerType, tags)
	numBlockedEvents := proc.statsFactory.NewTaggedStat("proc_num_tp_blocked_events", stats.CountType, tags)
	numOmittedProps := proc.statsFactory.NewTaggedStat("proc_num_tp_omitted_properties", stats.CountType, tags)
	numAllowedViolations := proc.statsFactory.NewTaggedStat("proc_num_tp_allowed_violations", stats.CountType, tags)

	return &TrackingPlanStatT{
		numEvents:                   numEvents,
		numValidationSuccessEvents:  numValidationSuccessEvents,
		numValidationFailedEvents:   numValidationFailedEvents,
		numValidationFilteredEvents: numValidationFilteredEvents,
		tpValidationTime:            tpValidationTime,
		numBlockedEvents:            numBlockedEvents,
		numOmittedProps:             numOmittedProps,
		numAllowedViolations:        numAllowedViolations,
	}
}
