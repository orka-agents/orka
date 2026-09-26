/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/responses"
	"github.com/orka-agents/orka/internal/llm"
)

func responsesHaveRefusal(output []responses.ResponseOutputItemUnion) bool {
	for _, item := range output {
		for _, part := range item.Content {
			if part.Type == stopReasonRefusal {
				return true
			}
		}
	}
	return false
}

func responsesEventHasRefusal(evt responses.ResponseStreamEventUnion) bool {
	return evt.Type == "response.refusal.delta" || evt.Type == "response.refusal.done" ||
		evt.Part.Type == stopReasonRefusal || responsesHaveRefusal([]responses.ResponseOutputItemUnion{evt.Item}) || responsesHaveRefusal(evt.Response.Output)
}

// This validation belongs only to callers of Orka's Responses endpoint.
// Other provider consumers retain their existing normalization policy.
func validateResponsesItemType(item responses.ResponseOutputItemUnion) error {
	switch item.Type {
	case eventTypeFunctionCall:
		return nil
	case responseOutputTypeMessage:
		if invalidResponseMetadataString(item.Role, item.JSON.Role) || (item.Role != "" && item.Role != messageRoleAssistant) {
			return fmt.Errorf("provider message role is outside the Responses subset")
		}
		for _, part := range item.Content {
			if part.Type != responseContentTypeOutputText && part.Type != stopReasonRefusal {
				return fmt.Errorf("provider message content is outside the Responses subset")
			}
		}
		return nil
	default:
		return fmt.Errorf("provider output item is outside the Responses subset")
	}
}

func validateResponsesOutput(output []responses.ResponseOutputItemUnion) error {
	var order responseOutputOrder
	calls := map[string]bool{}
	for i, item := range output {
		if err := validateResponsesItemType(item); err != nil {
			return err
		}
		if err := order.bind(item.ID, int64(i), item.Type, item.Status); err != nil {
			return err
		}
		if item.Type == eventTypeFunctionCall {
			if err := validateResponseFunctionMetadata(item); err != nil {
				return err
			}
			if item.Status != "" && item.Status != stopReasonCompleted {
				return fmt.Errorf("provider returned an unfinished Responses function call")
			}
			var arguments map[string]any
			if item.CallID == "" || item.Name == "" || calls[item.CallID] ||
				json.Unmarshal([]byte(responseOutputArguments(item.Arguments)), &arguments) != nil || arguments == nil {
				return fmt.Errorf("provider returned an invalid Responses function call")
			}
			calls[item.CallID] = true
		}
	}
	return nil
}

// Compatible snapshots may omit required identities for recovery.
// Explicitly cleared or mistyped values cannot restore an earlier identity.
func invalidResponseMetadataString(value string, field respjson.Field) bool {
	return field.Raw() != "" && (!field.Valid() || value == "")
}

func validateResponseFunctionMetadata(item responses.ResponseOutputItemUnion) error {
	if invalidResponseMetadataString(item.CallID, item.JSON.CallID) || invalidResponseMetadataString(item.Name, item.JSON.Name) {
		return fmt.Errorf("response function call has invalid metadata")
	}
	if item.JSON.Arguments.Raw() != "" && !item.JSON.Arguments.Valid() {
		return fmt.Errorf("response function call has invalid arguments")
	}
	return nil
}

func validateResponseFunctionEventMetadata(evt responses.ResponseStreamEventUnion) error {
	if evt.JSON.OutputIndex.Raw() != "" && (!evt.JSON.OutputIndex.Valid() || evt.OutputIndex < 0) {
		return fmt.Errorf("response function call has an invalid output index")
	}
	if evt.Type == eventTypeResponseFunctionCallArgumentsDelta || evt.Type == eventTypeResponseFunctionCallArgumentsDone {
		if invalidResponseMetadataString(evt.ItemID, evt.JSON.ItemID) || invalidResponseMetadataString(evt.Name, evt.JSON.Name) {
			return fmt.Errorf("response function call has invalid metadata")
		}
		arguments := evt.JSON.Arguments
		if evt.Type == eventTypeResponseFunctionCallArgumentsDelta {
			arguments = evt.JSON.Delta
		}
		if arguments.Raw() != "" && !arguments.Valid() {
			return fmt.Errorf("response function call has invalid arguments")
		}
		return nil
	}
	return validateResponseFunctionMetadata(evt.Item)
}

func (t *responseFuncCallTracker) getChecked(itemID string, index int64, hasIndex bool, callID string) (*responseFuncCallState, error) {
	if t.ordered {
		if itemID == "" && !hasIndex && callID == "" {
			return nil, fmt.Errorf("response function call has no identity or output index")
		}
		states := []*responseFuncCallState{
			{itemID: itemID, outputIndex: index, hasOutputIndex: hasIndex, callID: callID},
			t.byItemID[itemID], t.byCallID[callID],
		}
		if hasIndex {
			states = append(states, t.byOutputIndex[index])
		}
		// Validate every partial state before get merges or mutates any of
		// them. The incoming event may bridge previously separate records.
		for i, state := range states {
			for _, other := range states[:i] {
				if err := validateResponseFunctionMerge(state, other); err != nil {
					return nil, err
				}
			}
		}
	}
	return t.get(itemID, index, hasIndex, callID), nil
}

func validateResponseFunctionMerge(a, b *responseFuncCallState) error {
	if a == nil || b == nil || a == b {
		return nil
	}
	if (a.itemID != "" && b.itemID != "" && a.itemID != b.itemID) ||
		(a.callID != "" && b.callID != "" && a.callID != b.callID) ||
		(a.hasOutputIndex && b.hasOutputIndex && a.outputIndex != b.outputIndex) {
		return fmt.Errorf("response function call changed identity")
	}
	if a.name != "" && b.name != "" && a.name != b.name {
		return fmt.Errorf("response function call changed name")
	}
	aPrefix, bPrefix := a.args.String(), b.args.String()
	if !strings.HasPrefix(aPrefix, bPrefix) && !strings.HasPrefix(bPrefix, aPrefix) {
		return errors.New(responseFunctionArgumentsChanged)
	}
	if (a.argumentsDone && !strings.HasPrefix(a.arguments, bPrefix)) ||
		(b.argumentsDone && !strings.HasPrefix(b.arguments, aPrefix)) {
		return errors.New(responseFunctionArgumentsChanged)
	}
	if a.argumentsDone && b.argumentsDone && a.arguments != b.arguments {
		return errors.New(responseFunctionArgumentsChanged)
	}
	return nil
}

func (t *responseFuncCallTracker) validateSnapshot(fc *responseFuncCallState, name, arguments string, hasArguments bool) error {
	if !t.ordered || fc == nil {
		return nil
	}
	if name != "" && fc.name != "" && name != fc.name {
		return fmt.Errorf("response function call changed name")
	}
	if hasArguments && !strings.HasPrefix(arguments, fc.args.String()) {
		return errors.New(responseFunctionArgumentsChanged)
	}
	if fc.argumentsDone && hasArguments && arguments != fc.arguments {
		return errors.New(responseFunctionArgumentsChanged)
	}
	return nil
}

func failResponsesStream(send streamSender, err error) bool {
	send(llm.StreamChunk{Error: err, Done: true})
	return false
}

func (t *responseFuncCallTracker) prepareEvent(evt *responses.ResponseStreamEventUnion) error {
	switch evt.Type {
	case eventTypeResponseOutputItemAdded, eventTypeResponseOutputItemDone:
		if err := validateResponsesItemType(evt.Item); err != nil {
			return err
		}
		if evt.Item.Type == eventTypeFunctionCall {
			if err := validateResponseFunctionEventMetadata(*evt); err != nil {
				return err
			}
		}
		// Reject an unfinished item before exposing an executable call, even
		// if the terminal snapshot later omits that status.
		if evt.Type == eventTypeResponseOutputItemDone && evt.Item.Type == eventTypeFunctionCall &&
			evt.Item.Status != "" && evt.Item.Status != stopReasonCompleted {
			return fmt.Errorf("provider returned an unfinished Responses function call")
		}
	case eventTypeResponseFunctionCallArgumentsDelta, eventTypeResponseFunctionCallArgumentsDone:
		return validateResponseFunctionEventMetadata(*evt)
	case eventTypeResponseContentPartAdded, eventTypeResponseContentPartDone:
		if evt.Part.Type != responseContentTypeOutputText && evt.Part.Type != stopReasonRefusal {
			return fmt.Errorf("provider message content is outside the Responses subset")
		}
	case eventTypeResponseCompleted, eventTypeResponseIncomplete:
		if (evt.Type == eventTypeResponseCompleted && evt.Response.Status != stopReasonCompleted) ||
			(evt.Type == eventTypeResponseIncomplete && evt.Response.Status != stopReasonIncomplete) {
			return fmt.Errorf("response terminal status does not match its event type")
		}
		// A terminal may omit fields already supplied by the same call.
		// Recover only absent fields, then validate the complete output.
		for i, item := range evt.Response.Output {
			if item.Type != eventTypeFunctionCall {
				continue
			}
			if err := validateResponseFunctionMetadata(item); err != nil {
				return err
			}
			fc, err := t.getChecked(item.ID, int64(i), true, item.CallID)
			if err != nil {
				return err
			}
			if err := t.mergeItem(fc, item, true); err != nil {
				return err
			}
			if item.Name == "" {
				item.Name = fc.name
			}
			if item.CallID == "" {
				item.CallID = fc.callID
			}
			if responseOutputArguments(item.Arguments) == "" && item.JSON.Arguments.Raw() == "" {
				item.Arguments.OfString = fc.arguments
			}
			evt.Response.Output[i] = item
		}
		return validateResponsesOutput(evt.Response.Output)
	}
	return nil
}
