package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	orkaclient "github.com/orka-agents/orka/internal/cli/client"
)

const defaultMemoryWaitTimeout = 5 * time.Minute

type memoryMutationOptions struct {
	idempotencyKey string
	wait           bool
	waitTimeout    time.Duration
}

type memoryOperationWaitError struct {
	Location       string
	OperationID    string
	IdempotencyKey string
	Err            error
}

type memoryMutationRequestError struct {
	IdempotencyKey string
	Err            error
}

func (e *memoryMutationRequestError) Error() string {
	if e == nil {
		return "memory mutation request failed"
	}
	return fmt.Sprintf("memory mutation request failed: idempotency-key=%s: %v", e.IdempotencyKey, e.Err)
}

func (e *memoryMutationRequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *memoryOperationWaitError) Error() string {
	if e == nil {
		return "memory operation wait failed"
	}
	return fmt.Sprintf(
		"memory operation wait failed: operation=%s location=%s idempotency-key=%s: %v",
		e.OperationID,
		e.Location,
		e.IdempotencyKey,
		e.Err,
	)
}

func (e *memoryOperationWaitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func addMemoryMutationFlags(cmd *cobra.Command, options *memoryMutationOptions) {
	cmd.Flags().StringVar(
		&options.idempotencyKey,
		"idempotency-key",
		"",
		"Stable idempotency key for safe retries (generated when omitted)",
	)
	cmd.Flags().BoolVar(&options.wait, "wait", false, "Wait for deferred remote materialization to finish")
	cmd.Flags().DurationVar(
		&options.waitTimeout,
		"wait-timeout",
		defaultMemoryWaitTimeout,
		"Maximum time to wait for deferred materialization",
	)
}

func (o memoryMutationOptions) requestIdentity() (string, http.Header) {
	key := strings.TrimSpace(o.idempotencyKey)
	if key == "" {
		key = "mcli-" + uuid.NewString()
	}
	return key, http.Header{"Idempotency-Key": []string{key}}
}

func executeMemoryMutation(
	cmd *cobra.Command,
	client *orkaclient.Client,
	method, requestPath string,
	query map[string]string,
	body []byte,
	options memoryMutationOptions,
) (*orkaclient.JSONResponse, error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	idempotencyKey, headers := options.requestIdentity()
	response, err := client.DoJSONWithHeaders(ctx, method, requestPath, query, body, headers)
	if err != nil {
		requestErr := &memoryMutationRequestError{IdempotencyKey: idempotencyKey, Err: err}
		printMemoryMutationRequestFailure(cmd, requestErr)
		return nil, requestErr
	}
	if response.StatusCode != http.StatusAccepted || !options.wait {
		return response, nil
	}
	location := strings.TrimSpace(response.Header.Get("Location"))
	operationID := memoryOperationID(response.Body, location)
	if location == "" {
		waitErr := &memoryOperationWaitError{
			OperationID: operationID, IdempotencyKey: idempotencyKey,
			Err: fmt.Errorf("server returned 202 Accepted without Location"),
		}
		printMemoryWaitFailure(cmd, waitErr)
		return response, waitErr
	}
	waitTimeout := options.waitTimeout
	if waitTimeout <= 0 {
		waitTimeout = defaultMemoryWaitTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	waited, waitErr := waitForMemoryOperation(waitCtx, client, location, response.Header.Get("Retry-After"))
	if waitErr != nil {
		err := &memoryOperationWaitError{
			Location: location, OperationID: operationID, IdempotencyKey: idempotencyKey, Err: waitErr,
		}
		printMemoryWaitFailure(cmd, err)
		return response, err
	}
	return waited, nil
}

func printMemoryMutationRequestFailure(cmd *cobra.Command, err *memoryMutationRequestError) {
	if cmd == nil || err == nil {
		return
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Memory mutation context: idempotency-key=%s\n", err.IdempotencyKey)
}

func printMemoryWaitFailure(cmd *cobra.Command, err *memoryOperationWaitError) {
	if cmd == nil || err == nil {
		return
	}
	_, _ = fmt.Fprintf(
		cmd.ErrOrStderr(),
		"Memory operation context: location=%s operation=%s idempotency-key=%s\n",
		err.Location,
		err.OperationID,
		err.IdempotencyKey,
	)
}

func waitForMemoryOperation(
	ctx context.Context,
	client *orkaclient.Client,
	location, retryAfter string,
) (*orkaclient.JSONResponse, error) {
	delay := parseMemoryRetryAfter(retryAfter)
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for memory operation: %w", ctx.Err())
		case <-time.After(delay):
		}
		response, err := client.DoJSONWithHeaders(ctx, http.MethodGet, location, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		state := memoryOperationState(response.Body)
		switch state {
		case "succeeded":
			return response, nil
		case "dead_lettered", "abandoned", "superseded", "orphaned":
			return nil, fmt.Errorf("memory operation finished in %s state", state)
		}
		delay = parseMemoryRetryAfter(response.Header.Get("Retry-After"))
	}
}

func parseMemoryRetryAfter(raw string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err == nil && seconds > 0 {
		if seconds > 30 {
			seconds = 30
		}
		return time.Duration(seconds) * time.Second
	}
	return time.Second
}

func memoryOperationState(body any) string {
	object, ok := body.(map[string]any)
	if !ok {
		return ""
	}
	state, _ := object["state"].(string)
	return strings.ToLower(strings.TrimSpace(state))
}

func memoryOperationID(body any, location string) string {
	if identifier := metadataName(body); identifier != "" {
		return identifier
	}
	parsed, err := url.Parse(strings.TrimSpace(location))
	if err != nil {
		return ""
	}
	identifier := path.Base(strings.TrimRight(parsed.Path, "/"))
	if identifier == "." || identifier == "/" {
		return ""
	}
	return identifier
}

func memoryMutationResultSubject(body any, knownSubject string) string {
	object, ok := body.(map[string]any)
	if ok {
		if _, isOperation := object["state"]; isOperation {
			if memoryID := firstString(object, "memoryId"); memoryID != "" {
				return memoryID
			}
			return knownSubject
		}
	}
	if identifier := metadataName(body); identifier != "" {
		return identifier
	}
	return knownSubject
}

func printMemoryMutationOutcome(cmd *cobra.Command, action, subject string, response *orkaclient.JSONResponse) error {
	if response == nil {
		return fmt.Errorf("empty memory mutation response")
	}
	if response.StatusCode == http.StatusAccepted {
		operationID := memoryOperationID(response.Body, response.Header.Get("Location"))
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s accepted: %s\n", action, operationID)
		return err
	}
	identifier := memoryMutationResultSubject(response.Body, subject)
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", action, identifier)
	return err
}
