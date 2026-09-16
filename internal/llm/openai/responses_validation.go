/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package openai

import (
	"encoding/json"
	"errors"
	"fmt"

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
		if err := order.bind(item.ID, int64(i)); err != nil {
			return err
		}
		if item.Type == eventTypeFunctionCall {
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
	if a.argumentsDone && b.argumentsDone && a.arguments != b.arguments {
		return errors.New(responseFunctionArgumentsChanged)
	}
	return nil
}

func (t *responseFuncCallTracker) validateSnapshot(fc *responseFuncCallState, name, arguments string) error {
	if !t.ordered || fc == nil {
		return nil
	}
	if name != "" && fc.name != "" && name != fc.name {
		return fmt.Errorf("response function call changed name")
	}
	if fc.argumentsDone && arguments != "" && arguments != fc.arguments {
		return errors.New(responseFunctionArgumentsChanged)
	}
	return nil
}

func failResponsesStream(send streamSender, err error) bool {
	send(llm.StreamChunk{Error: err, Done: true})
	return false
}

func (t *responseFuncCallTracker) validateEvent(evt responses.ResponseStreamEventUnion) error {
	switch evt.Type {
	case eventTypeResponseOutputItemAdded, eventTypeResponseOutputItemDone:
		if err := validateResponsesItemType(evt.Item); err != nil {
			return err
		}
		// Function output is exposed as completed once its arguments arrive.
		// A later item-done status cannot retroactively make it unfinished,
		// even if the terminal snapshot omits that status.
		if evt.Type == eventTypeResponseOutputItemDone && evt.Item.Type == eventTypeFunctionCall &&
			evt.Item.Status != "" && evt.Item.Status != stopReasonCompleted {
			return fmt.Errorf("provider returned an unfinished Responses function call")
		}
	case "response.content_part.added", eventTypeResponseContentPartDone:
		if evt.Part.Type != responseContentTypeOutputText && evt.Part.Type != stopReasonRefusal {
			return fmt.Errorf("provider message content is outside the Responses subset")
		}
	case eventTypeResponseCompleted, eventTypeResponseIncomplete:
		if err := validateResponsesOutput(evt.Response.Output); err != nil {
			return err
		}
		for i, item := range evt.Response.Output {
			if item.Type == eventTypeFunctionCall {
				if _, err := t.getChecked(item.ID, int64(i), true, item.CallID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
