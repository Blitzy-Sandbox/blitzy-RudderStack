// Package processor — functions_adapter.go bridges the functions/runtime.Engine with the
// processor's internal interfaces for Functions pipeline integration (E-015, E-016, E-017).
//
// Two adapters are defined:
//
//  1. insertFunctionEngineAdapter — satisfies the unexported insertFunctionExecutor interface,
//     converting between the processor's local mirror types (insertFunctionDef, insertFunctionResult)
//     and functions/runtime types (FunctionDef, InsertFunctionResult).
//
//  2. functionsClientAdapter — satisfies the transformer.FunctionsClient interface, converting
//     between the processor/types request/response shapes and the functions/runtime Engine methods.
//
// Both adapters are constructed by the WithFunctionsRuntime Opts function in manager.go.
package processor

import (
	"context"
	"encoding/json"

	"github.com/rudderlabs/rudder-go-kit/jsonrs"

	functionsruntime "github.com/rudderlabs/rudder-server/functions/runtime"
	"github.com/rudderlabs/rudder-server/processor/types"
)

// ---------------------------------------------------------------------------
// insertFunctionEngineAdapter — satisfies insertFunctionExecutor
// ---------------------------------------------------------------------------

// insertFunctionEngineAdapter wraps *runtime.Engine and satisfies the processor-local
// insertFunctionExecutor interface. It converts between the processor's unexported mirror
// types (insertFunctionDef / insertFunctionResult) and the runtime's exported types
// (FunctionDef / InsertFunctionResult).
//
// The two type hierarchies are structurally identical but are distinct Go types,
// so explicit field-by-field copying is required.
type insertFunctionEngineAdapter struct {
	engine *functionsruntime.Engine
}

// ExecuteInsertFunction converts the processor-local insertFunctionDef to a runtime.FunctionDef,
// delegates to the runtime Engine, and converts the runtime.InsertFunctionResult back to the
// processor-local insertFunctionResult.
func (a *insertFunctionEngineAdapter) ExecuteInsertFunction(
	ctx context.Context,
	fn *insertFunctionDef,
	event json.RawMessage,
	settings map[string]string,
) (*insertFunctionResult, error) {
	// Convert processor mirror type → runtime type (field-by-field copy).
	runtimeDef := &functionsruntime.FunctionDef{
		ID:          fn.ID,
		WorkspaceID: fn.WorkspaceID,
		Name:        fn.Name,
		Code:        fn.Code,
		Version:     fn.Version,
		Type:        fn.Type,
		Settings:    fn.Settings,
	}

	runtimeResult, err := a.engine.ExecuteInsertFunction(ctx, runtimeDef, event, settings)
	if err != nil {
		return nil, err
	}

	// Convert runtime type → processor mirror type.
	return &insertFunctionResult{
		Event:   runtimeResult.Event,
		Dropped: runtimeResult.Dropped,
	}, nil
}

// ---------------------------------------------------------------------------
// functionsClientAdapter — satisfies transformer.FunctionsClient
// ---------------------------------------------------------------------------

// functionsClientAdapter wraps *runtime.Engine and satisfies the transformer.FunctionsClient
// interface, enabling the processor's destination function execution path (E-016) and any
// future transformer-level function calls to reach the Functions runtime engine.
type functionsClientAdapter struct {
	engine *functionsruntime.Engine
}

// ExecuteSourceFunction converts a types.FunctionRequest to a runtime.SourceFunctionRequest,
// delegates to the Engine, and converts the result back to types.FunctionResponse.
func (a *functionsClientAdapter) ExecuteSourceFunction(
	ctx context.Context,
	functionID string,
	request types.FunctionRequest,
) (types.FunctionResponse, error) {
	// Convert types.FunctionRequest → runtime.SourceFunctionRequest.
	// Headers: types.FunctionRequest uses map[string]string (single-valued);
	// runtime uses map[string][]string (multi-valued). Wrap each value in a slice.
	rtHeaders := make(map[string][]string, len(request.Headers))
	for k, v := range request.Headers {
		rtHeaders[k] = []string{v}
	}
	rtQueryParams := make(map[string][]string, len(request.QueryParams))
	for k, v := range request.QueryParams {
		rtQueryParams[k] = []string{v}
	}

	rtReq := &functionsruntime.SourceFunctionRequest{
		Method:      request.Method,
		URL:         request.URL,
		Headers:     rtHeaders,
		Body:        json.RawMessage(request.Body),
		QueryParams: rtQueryParams,
	}

	// Build a minimal FunctionDef from the functionID. Source Functions are looked up
	// by ID in the Functions management store; the adapter only needs the ID for dispatch.
	fnDef := &functionsruntime.FunctionDef{
		ID:   functionID,
		Type: functionsruntime.FunctionTypeSource,
	}

	// Convert settings from map[string]any to map[string]string for the runtime.
	rtSettings := make(map[string]string, len(request.Settings))
	for k, v := range request.Settings {
		if s, ok := v.(string); ok {
			rtSettings[k] = s
		}
	}

	result, err := a.engine.ExecuteSourceFunction(ctx, fnDef, rtReq, rtSettings)
	if err != nil {
		return types.FunctionResponse{
			StatusCode: 500,
			Error:      err.Error(),
		}, err
	}

	// Convert runtime.SourceFunctionResult → types.FunctionResponse.
	var events []types.SingularEventT
	for _, rawEvt := range result.Events {
		var evt types.SingularEventT
		if unmarshalErr := jsonrs.Unmarshal(rawEvt, &evt); unmarshalErr == nil {
			events = append(events, evt)
		}
	}

	return types.FunctionResponse{
		Events:     events,
		StatusCode: 200,
	}, nil
}

// ExecuteDestinationFunction converts []types.TransformerEvent to individual json.RawMessage
// events, executes the destination function via the Engine for each event, and aggregates
// results into a types.Response.
func (a *functionsClientAdapter) ExecuteDestinationFunction(
	ctx context.Context,
	functionID string,
	eventType string,
	events []types.TransformerEvent,
) types.Response {
	var resp types.Response

	fnDef := &functionsruntime.FunctionDef{
		ID:   functionID,
		Type: functionsruntime.FunctionTypeDestination,
	}

	for i := range events {
		eventJSON, marshalErr := jsonrs.Marshal(events[i].Message)
		if marshalErr != nil {
			resp.FailedEvents = append(resp.FailedEvents, types.TransformerResponse{
				Output:     events[i].Message,
				Metadata:   events[i].Metadata,
				StatusCode: 500,
				Error:      marshalErr.Error(),
			})
			continue
		}

		result, execErr := a.engine.ExecuteDestinationFunction(
			ctx, fnDef, json.RawMessage(eventJSON), eventType, nil,
		)
		if execErr != nil {
			resp.FailedEvents = append(resp.FailedEvents, types.TransformerResponse{
				Output:     events[i].Message,
				Metadata:   events[i].Metadata,
				StatusCode: 500,
				Error:      execErr.Error(),
			})
			continue
		}

		// Convert result back to TransformerResponse.
		var outputMap map[string]any
		if result.Body != nil {
			_ = jsonrs.Unmarshal(result.Body, &outputMap)
		}
		if outputMap == nil {
			// If no body or unmarshal fails, pass through the original event.
			outputMap = events[i].Message
		}

		resp.Events = append(resp.Events, types.TransformerResponse{
			Output:     outputMap,
			Metadata:   events[i].Metadata,
			StatusCode: result.StatusCode,
		})
	}

	return resp
}

// ExecuteInsertFunction converts []types.TransformerEvent to individual json.RawMessage events,
// executes the insert function via the Engine, and aggregates results into a types.Response.
func (a *functionsClientAdapter) ExecuteInsertFunction(
	ctx context.Context,
	functionID string,
	events []types.TransformerEvent,
) types.Response {
	var resp types.Response

	fnDef := &functionsruntime.FunctionDef{
		ID:   functionID,
		Type: functionsruntime.FunctionTypeInsert,
	}

	for i := range events {
		eventJSON, marshalErr := jsonrs.Marshal(events[i].Message)
		if marshalErr != nil {
			resp.FailedEvents = append(resp.FailedEvents, types.TransformerResponse{
				Output:     events[i].Message,
				Metadata:   events[i].Metadata,
				StatusCode: 500,
				Error:      marshalErr.Error(),
			})
			continue
		}

		result, execErr := a.engine.ExecuteInsertFunction(
			ctx, fnDef, json.RawMessage(eventJSON), nil,
		)
		if execErr != nil {
			resp.FailedEvents = append(resp.FailedEvents, types.TransformerResponse{
				Output:     events[i].Message,
				Metadata:   events[i].Metadata,
				StatusCode: 500,
				Error:      execErr.Error(),
			})
			continue
		}

		if result.Dropped {
			// Dropped events are not added to the response — they are removed from the pipeline.
			continue
		}

		var outputMap map[string]any
		if result.Event != nil {
			_ = jsonrs.Unmarshal(result.Event, &outputMap)
		}
		if outputMap == nil {
			outputMap = events[i].Message
		}

		resp.Events = append(resp.Events, types.TransformerResponse{
			Output:     outputMap,
			Metadata:   events[i].Metadata,
			StatusCode: 200,
		})
	}

	return resp
}
