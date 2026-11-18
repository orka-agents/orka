/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/memoryconsolidation"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	roleAssistant = "assistant"
	roleTool      = "tool"

	memoryConsolidationResponseLimit = 8 << 20
	memoryConsolidationHTTPTimeout   = 2 * time.Minute
	// memoryConsolidationModelAttempts bounds how often a run asks the model
	// for its structured result.
	memoryConsolidationModelAttempts = 2
	// memoryConsolidationUnavailableAttempts bounds how often a call is
	// retried while the controller answers 503, as it does until it has
	// published the worker's Job.
	memoryConsolidationUnavailableAttempts = 12
)

// memoryConsolidationRetryDelay spaces retries of a 503; tests shorten it.
var memoryConsolidationRetryDelay = 5 * time.Second

// errControllerUnavailable marks a 503 from the controller.
var errControllerUnavailable = errors.New("controller unavailable")

// memoryConsolidationClient calls the controller's internal run routes as
// this worker's Task.
type memoryConsolidationClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newMemoryConsolidationClient() (*memoryConsolidationClient, error) {
	controllerURL := strings.TrimRight(strings.TrimSpace(os.Getenv(workerenv.ControllerURL)), "/")
	namespace := strings.TrimSpace(os.Getenv(workerenv.TaskNamespace))
	if controllerURL == "" || namespace == "" {
		return nil, fmt.Errorf("%s and %s are required for a memory consolidation run",
			workerenv.ControllerURL, workerenv.TaskNamespace)
	}
	return &memoryConsolidationClient{
		baseURL: controllerURL + "/internal/v1/memory-consolidation/" + url.PathEscape(namespace),
		token:   workerServiceAccountToken(),
		http:    &http.Client{Timeout: memoryConsolidationHTTPTimeout},
	}, nil
}

// post calls a run route, retrying while the controller answers 503.
func (c *memoryConsolidationClient) post(ctx context.Context, path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		err = c.postOnce(ctx, path, encoded, out)
		if !errors.Is(err, errControllerUnavailable) || attempt == memoryConsolidationUnavailableAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(memoryConsolidationRetryDelay):
		}
	}
}

func (c *memoryConsolidationClient) postOnce(ctx context.Context, path string, encoded []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(resp.Body, memoryConsolidationResponseLimit))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("%w: memory consolidation %s returned HTTP 503: %s",
			errControllerUnavailable, path, strings.TrimSpace(string(data)))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("memory consolidation %s returned HTTP %d: %s",
			path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

// runMemoryConsolidation runs this Task as a memory consolidation run: the
// controller supplies the inputs, the model submits one structured result
// through the only tool it is offered, and the controller validates it and
// records the report, which becomes the Task result. No other tool runs and
// no memory is injected.
func runMemoryConsolidation(
	ctx context.Context, provider llm.Provider, model string, settings modelSettings, focus string,
) (string, error) {
	client, err := newMemoryConsolidationClient()
	if err != nil {
		return "", err
	}
	var begin memoryconsolidation.BeginResponse
	if err := client.post(ctx, "/begin", struct{}{}, &begin); err != nil {
		return "", fmt.Errorf("begin memory consolidation: %w", err)
	}
	if begin.Status != memoryconsolidation.StatusRunning || begin.Input == nil {
		// A skipped run, or a retried Pod of a finished one.
		return marshalConsolidationReport(begin.Report)
	}
	fmt.Printf("Memory consolidation run reads %d memories and %d of %d changed sessions\n",
		len(begin.Input.Memories), len(begin.Input.Sessions), begin.Input.EligibleSessions)

	submission, err := requestConsolidation(ctx, provider, model, settings, *begin.Input, focus, begin.Settings)
	if err != nil {
		return "", err
	}
	var report memoryconsolidation.Report
	if err := client.post(ctx, "/complete", submission, &report); err != nil {
		return "", fmt.Errorf("complete memory consolidation: %w", err)
	}
	return marshalConsolidationReport(&report)
}

// requestConsolidation asks the model for its structured result, once more
// with a reminder when it answers without calling the tool.
func requestConsolidation(
	ctx context.Context, provider llm.Provider, model string, settings modelSettings,
	input memoryconsolidation.Input, focus string, runSettings memoryconsolidation.Settings,
) (memoryconsolidation.Submission, error) {
	userMessage, err := memoryconsolidation.UserMessage(input, focus, runSettings)
	if err != nil {
		return memoryconsolidation.Submission{}, err
	}
	messages := []llm.Message{{Role: roleUser, Content: userMessage}}
	tool := llm.Tool{Name: memoryconsolidation.SubmitToolName, Description: memoryconsolidation.SubmitToolDescription,
		Parameters: memoryconsolidation.SubmitToolParameters}
	for attempt := range memoryConsolidationModelAttempts {
		resp, err := provider.Complete(ctx, &llm.CompletionRequest{
			Model: model, Messages: messages, SystemPrompt: memoryconsolidation.SystemPrompt,
			SystemPromptStable: len(memoryconsolidation.SystemPrompt),
			MaxTokens:          settings.maxTokens, Temperature: settings.temperature, TemperatureSet: settings.temperatureSet,
			Tools: []llm.Tool{tool},
		})
		if err := completionCallError(resp, err); err != nil {
			return memoryconsolidation.Submission{}, fmt.Errorf("memory consolidation model call: %w", err)
		}
		// An unfinished, refused or failed response may still carry a tool
		// call with valid arguments; submitting it would propose from a
		// result the model never finished.
		switch outcome := llm.NormalizeCompletionOutcome(resp); outcome {
		case llm.CompletionOutcomeCompleted, llm.CompletionOutcomeToolCalls:
		default:
			return memoryconsolidation.Submission{}, fmt.Errorf("memory consolidation model call: %w",
				completionOutcomeError(outcome, resp.StopReason))
		}
		for _, call := range resp.ToolCalls {
			if call.Name != memoryconsolidation.SubmitToolName {
				continue
			}
			var submission memoryconsolidation.Submission
			if err := json.Unmarshal(call.Arguments, &submission); err != nil {
				return memoryconsolidation.Submission{}, fmt.Errorf("decode memory consolidation result: %w", err)
			}
			if submission.Operations == nil {
				// Not a result: an empty list says nothing changes, a missing
				// one says nothing. Ask again rather than finish the run.
				break
			}
			return submission, nil
		}
		if attempt+1 < memoryConsolidationModelAttempts {
			messages = append(messages, consolidationReminder(resp)...)
		}
	}
	return memoryconsolidation.Submission{}, errors.New("the model did not submit a memory consolidation result")
}

const consolidationReminderText = "Call " + memoryconsolidation.SubmitToolName +
	" exactly once with your result, including its operations list." +
	" Submit an empty operations list if memory needs no change."

// consolidationReminder replays the model's turn and asks again. A turn that
// called tools is answered with a result for each call, which carries the
// reminder: providers reject an assistant turn with no text and no calls,
// and every call needs its result.
func consolidationReminder(resp *llm.CompletionResponse) []llm.Message {
	if len(resp.ToolCalls) > 0 {
		messages := make([]llm.Message, 0, 1+len(resp.ToolCalls))
		messages = append(messages, llm.Message{Role: roleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})
		for _, call := range resp.ToolCalls {
			messages = append(messages, llm.Message{Role: roleTool, Content: "Not accepted. " + consolidationReminderText,
				ToolCallID: call.ID, Name: call.Name})
		}
		return messages
	}
	if strings.TrimSpace(resp.Content) == "" {
		return []llm.Message{{Role: roleUser, Content: consolidationReminderText}}
	}
	return []llm.Message{
		{Role: roleAssistant, Content: resp.Content},
		{Role: roleUser, Content: consolidationReminderText},
	}
}

func marshalConsolidationReport(report *memoryconsolidation.Report) (string, error) {
	if report == nil {
		return "", errors.New("memory consolidation run has no report")
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
