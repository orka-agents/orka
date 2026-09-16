/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/orka-agents/orka/internal/llm"
)

// ResponsesRequest is the stateless, text-only subset served by Orka. Unknown
// fields fail closed so a client cannot mistake ignored state for saved history.
type ResponsesRequest struct {
	Model              string               `json:"model"`
	Include            []string             `json:"include,omitempty"`
	Input              json.RawMessage      `json:"input"`
	Instructions       string               `json:"instructions,omitempty"`
	Store              *bool                `json:"store"`
	Stream             bool                 `json:"stream,omitempty"`
	Temperature        *float64             `json:"temperature,omitempty"`
	MaxOutputTokens    *int                 `json:"max_output_tokens,omitempty"`
	Text               *responsesTextConfig `json:"text,omitempty"`
	Tools              []responsesFunction  `json:"tools,omitempty"`
	ToolChoice         json.RawMessage      `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID json.RawMessage      `json:"previous_response_id,omitempty"`
	Conversation       json.RawMessage      `json:"conversation,omitempty"`
}

type responsesTextConfig struct {
	Format responsesTextFormat `json:"format"`
}

type responsesTextFormat struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
	Description string         `json:"description,omitempty"`
}

type responsesFunction struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type responsesInputItem struct {
	Type      string          `json:"type,omitempty"`
	ID        string          `json:"id,omitempty"`
	Status    string          `json:"status,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

type responsesContentPart struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations,omitempty"`
	Logprobs    []json.RawMessage `json:"logprobs,omitempty"`
}

func decodeResponsesJSON(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func responsesInvalid(param, message string) *OAIErrorDetail {
	return &OAIErrorDetail{Type: OAIErrorTypeInvalidRequest, Param: &param, Message: message}
}

func parseResponsesRequest(body []byte) (*ResponsesRequest, *llm.CompletionRequest, *OAIErrorDetail) {
	var req ResponsesRequest
	if err := decodeResponsesJSON(body, &req); err != nil {
		return nil, nil, responsesInvalid("body", "invalid or unsupported Responses request field: "+err.Error())
	}
	if len(req.Include) != 0 {
		return nil, nil, responsesInvalid("include", "additional output fields, including reasoning, are unsupported; use include:[]")
	}
	if req.Store == nil || *req.Store {
		return nil, nil, responsesInvalid("store", "Responses requires store:false; history must be supplied by the client")
	}
	// Even null is rejected: these fields are not part of the stateless contract.
	if len(req.PreviousResponseID) != 0 {
		return nil, nil, responsesInvalid("previous_response_id", "previous_response_id is unsupported; include history in input")
	}
	if len(req.Conversation) != 0 {
		return nil, nil, responsesInvalid("conversation", "saved conversations are unsupported; include history in input")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, nil, responsesInvalid("model", "model is required")
	}
	if len(req.ToolChoice) != 0 {
		var choice string
		if err := json.Unmarshal(req.ToolChoice, &choice); err != nil || choice != "auto" {
			return nil, nil, responsesInvalid("tool_choice", "only tool_choice:auto is supported")
		}
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		return nil, nil, responsesInvalid("parallel_tool_calls", "parallel_tool_calls:false is not supported")
	}
	comp := &llm.CompletionRequest{Model: req.Model, SystemPrompt: req.Instructions, Store: req.Store, ResponsesInput: true}
	if req.Temperature != nil {
		if *req.Temperature < 0 || *req.Temperature > 2 {
			return nil, nil, responsesInvalid("temperature", "temperature must be between 0 and 2")
		}
		comp.Temperature = *req.Temperature
		comp.TemperatureSet = true
	}
	if req.MaxOutputTokens != nil {
		if *req.MaxOutputTokens < 1 {
			return nil, nil, responsesInvalid("max_output_tokens", "max_output_tokens must be positive")
		}
		comp.MaxTokens = *req.MaxOutputTokens
	}
	if invalid := applyResponsesFormat(comp, req.Text); invalid != nil {
		return nil, nil, invalid
	}
	names := map[string]bool{}
	for _, tool := range req.Tools {
		if tool.Type != responsesFunctionToolType {
			return nil, nil, responsesInvalid("tools", "only function tools are supported; OpenAI-hosted tools are unavailable")
		}
		if tool.Name == "" || names[tool.Name] {
			return nil, nil, responsesInvalid("tools", "function names must be non-empty and unique")
		}
		names[tool.Name] = true
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var schema map[string]any
		if err := json.Unmarshal(parameters, &schema); err != nil || schema == nil {
			return nil, nil, responsesInvalid("tools", "function parameters must be a JSON Schema object")
		}
		comp.Tools = append(comp.Tools, llm.Tool{Name: tool.Name, Description: tool.Description, Parameters: parameters, Strict: tool.Strict})
	}
	messages, err := convertResponsesInput(req.Input)
	if err != nil {
		return nil, nil, responsesInvalid("input", err.Error())
	}
	comp.Messages = messages
	return &req, comp, nil
}

func responsesInputText(content json.RawMessage, output bool) (string, error) {
	var text string
	if err := json.Unmarshal(content, &text); err == nil && string(content) != responsesJSONNull {
		return text, nil
	}
	var parts []responsesContentPart
	if err := decodeResponsesJSON(content, &parts); err != nil || len(parts) == 0 {
		return "", fmt.Errorf("content must be a string or non-empty text content array")
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Type != "input_text" && (!output || part.Type != "output_text") {
			return "", fmt.Errorf("only text content is supported")
		}
		if len(part.Annotations) != 0 || len(part.Logprobs) != 0 {
			return "", fmt.Errorf("annotations and logprobs are unsupported")
		}
		result.WriteString(part.Text)
	}
	return result.String(), nil
}

func convertResponsesInput(input json.RawMessage) ([]llm.Message, error) {
	var text string
	if err := json.Unmarshal(input, &text); err == nil && string(input) != responsesJSONNull {
		return []llm.Message{{Role: chatRoleUser, Content: text}}, nil
	}
	var items []responsesInputItem
	if err := decodeResponsesJSON(input, &items); err != nil || len(items) == 0 {
		return nil, fmt.Errorf("input must be a string or non-empty input item array")
	}
	messages := make([]llm.Message, 0, len(items))
	calls := map[string]string{}
	pending := map[string]bool{}
	for _, item := range items {
		if item.Status != "" && item.Status != completionStatusCompleted {
			return nil, fmt.Errorf("only completed history items are supported")
		}
		switch item.Type {
		case "", responsesMessage:
			var err error
			messages, err = appendResponsesMessage(messages, item, len(pending) != 0)
			if err != nil {
				return nil, err
			}
		case finishReasonFunctionCall:
			if item.Role != "" || len(item.Content) != 0 || len(item.Output) != 0 {
				return nil, fmt.Errorf("invalid function_call fields")
			}
			if item.CallID == "" || item.Name == "" || calls[item.CallID] != "" {
				return nil, fmt.Errorf("function_call requires unique call_id and name")
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(item.Arguments), &args); err != nil || args == nil {
				return nil, fmt.Errorf("function_call arguments must encode a JSON object")
			}
			calls[item.CallID] = item.Name
			pending[item.CallID] = true
			call := llm.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)}
			if len(messages) > 0 && messages[len(messages)-1].Role == responsesRoleAssistant && len(messages[len(messages)-1].ToolCalls) > 0 {
				messages[len(messages)-1].ToolCalls = append(messages[len(messages)-1].ToolCalls, call)
			} else {
				messages = append(messages, llm.Message{Role: responsesRoleAssistant, ToolCalls: []llm.ToolCall{call}})
			}
		case "function_call_output":
			if item.Role != "" || len(item.Content) != 0 || item.Name != "" || item.Arguments != "" {
				return nil, fmt.Errorf("invalid function_call_output fields")
			}
			if !pending[item.CallID] {
				return nil, fmt.Errorf("function_call_output requires a matching preceding call_id")
			}
			result, err := responsesInputText(item.Output, false)
			if err != nil {
				return nil, err
			}
			delete(pending, item.CallID)
			messages = append(messages, llm.Message{Role: responsesRoleTool, ToolCallID: item.CallID, Name: calls[item.CallID], Content: result})
		default:
			return nil, fmt.Errorf("unsupported input item type %q; reasoning, hosted tools and saved item references are unavailable", item.Type)
		}
	}
	if len(pending) != 0 {
		return nil, fmt.Errorf("history contains function calls without results")
	}
	return messages, nil
}

func applyResponsesFormat(comp *llm.CompletionRequest, text *responsesTextConfig) *OAIErrorDetail {
	if text != nil {
		format := text.Format
		switch format.Type {
		case oaiContentTypeText, "json_object":
			if format.Name != "" || format.Schema != nil || format.Strict != nil || format.Description != "" {
				return responsesInvalid("text.format", "schema fields require type:json_schema")
			}
		case "json_schema":
			if format.Name == "" || format.Schema == nil {
				return responsesInvalid("text.format", "json_schema requires name and schema")
			}
		default:
			return responsesInvalid("text.format.type", "supported formats are text, json_object and json_schema")
		}
		comp.ResponseFormat = &llm.ResponseFormat{Type: format.Type}
		if format.Type == "json_schema" {
			comp.ResponseFormat.JSONSchema = &llm.JSONSchemaFormat{Name: format.Name, Schema: format.Schema, Strict: format.Strict, Description: format.Description}
		}
	}
	return nil
}

func appendResponsesMessage(messages []llm.Message, item responsesInputItem, hasPending bool) ([]llm.Message, error) {
	if hasPending && item.Role != responsesRoleAssistant {
		return nil, fmt.Errorf("function calls require results before the next user or system message")
	}
	if item.CallID != "" || item.Name != "" || item.Arguments != "" || len(item.Output) != 0 {
		return nil, fmt.Errorf("function fields are invalid on messages")
	}
	role := item.Role
	switch role {
	case chatRoleUser, responsesRoleAssistant, oaiRoleSystem:
	case "developer":
		role = oaiRoleSystem
	default:
		return nil, fmt.Errorf("unsupported message role")
	}
	content, err := responsesInputText(item.Content, role == responsesRoleAssistant)
	if err != nil {
		return nil, err
	}
	// Keep text and function items distinct so Responses providers preserve
	// the input order. Providers with turn-based formats normalize at their edge.
	messages = append(messages, llm.Message{Role: role, Content: content})

	return messages, nil
}
