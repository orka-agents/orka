package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestBoundedModelEnvironmentFailsClosed(t *testing.T) {
	values := map[string]string{
		contract.BoundedModelRequestsEnv: "1",
		contract.ModelBudgetRunEnv:       "run-uid",
		contract.ModelBudgetRoleEnv:      "checks",
		contract.ModelBudgetDeadlineEnv:  time.Now().Add(time.Minute).Format(time.RFC3339Nano),
		workerenv.MemoryContextEnabled:   "false",
		workerenv.MemoryToolsAutoEnable:  "false",
	}
	getenv := func(name string) string { return values[name] }
	bounded, err := boundedModelRequests(getenv)
	require.NoError(t, err)
	require.True(t, bounded)
	worker := workerenv.AIWorkerEnv{
		BaseEnv: workerenv.BaseEnv{TaskName: "proposal", TaskNamespace: "private", TaskUID: "task-uid"},
	}
	require.NoError(t, validateBoundedModelEnvironment(getenv, worker))
	for _, invalid := range []string{"0", "2", "10", "-1", " 1", "invalid"} {
		values[contract.BoundedModelRequestsEnv] = invalid
		_, err := boundedModelRequests(getenv)
		require.Error(t, err)
	}
	delete(values, contract.BoundedModelRequestsEnv)
	_, err = boundedModelRequests(getenv)
	require.Error(t, err, "an incomplete role budget cannot silently select legacy behavior")
	values[contract.BoundedModelRequestsEnv] = "1"
	deadline := values[contract.ModelBudgetDeadlineEnv]
	delete(values, contract.ModelBudgetDeadlineEnv)
	_, err = boundedModelRequests(getenv)
	require.Error(t, err, "an invocation cannot lose its absolute run deadline")
	values[contract.ModelBudgetDeadlineEnv] = deadline
	values[workerenv.MemoryContextEnabled] = ""
	require.Error(t, validateBoundedModelEnvironment(getenv, worker))
	values[workerenv.MemoryContextEnabled] = "false"
	worker.Tools = []string{"file_read"}
	require.Error(t, validateBoundedModelEnvironment(getenv, worker))
	worker.Tools = nil
	worker.Fallbacks = []workerenv.FallbackProviderEnv{{Provider: "openai"}}
	require.Error(t, validateBoundedModelEnvironment(getenv, worker))
}

func TestModelTransportConsumesPermitBeforeOneConcurrentAttempt(t *testing.T) {
	var claims, sent atomic.Int32
	transport := &modelBudgetTransport{
		claim: func(context.Context) error { claims.Add(1); return nil },
		send: func(request *http.Request) (*http.Response, error) {
			sent.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("{}")), Request: request}, nil
		},
	}
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://model.invalid/v1/responses", nil)
			require.NoError(t, err)
			response, err := transport.RoundTrip(request)
			if err == nil {
				require.NoError(t, response.Body.Close())
			} else {
				require.ErrorIs(t, err, errModelCallBudget)
			}
		})
	}
	workers.Wait()
	require.EqualValues(t, 1, claims.Load())
	require.EqualValues(t, 1, sent.Load())
}

func TestModelTransportNeverSendsAfterUncertainPermit(t *testing.T) {
	var sent atomic.Int32
	transport := &modelBudgetTransport{
		claim: func(context.Context) error { return errors.New("uncertain acknowledgement") },
		send:  func(*http.Request) (*http.Response, error) { sent.Add(1); return nil, nil },
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://model.invalid/v1/responses", nil)
	require.NoError(t, err)
	for range 2 {
		_, err := transport.RoundTrip(request.Clone(t.Context()))
		require.ErrorIs(t, err, errModelCallBudget)
	}
	require.Zero(t, sent.Load())
}

func TestSDKRetriesAndAPIFallbackCannotResetModelBudget(t *testing.T) {
	for _, providerName := range []string{"openai", "azure-openai", "anthropic"} {
		for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusNotFound} {
			t.Run(providerName+"/"+http.StatusText(code), func(t *testing.T) {
				var sent, claims atomic.Int32
				client := boundedModelClient(func(context.Context) error { claims.Add(1); return nil })
				client.Transport.(*modelBudgetTransport).send = func(request *http.Request) (*http.Response, error) {
					sent.Add(1)
					return &http.Response{
						StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
						Body: io.NopCloser(strings.NewReader(
							`{"error":{"message":"unsupported API",` +
								`"code":"unsupported_api","type":"api_error"}}`,
						)),
						Request: request,
					}, nil
				}
				provider, err := llm.NewProvider(providerName, llm.ProviderConfig{
					APIKey: "synthetic-placeholder", BaseURL: "https://model.invalid/", ProviderType: providerName,
					HTTPClient: client, DisableRetries: true,
				})
				require.NoError(t, err)
				request := &llm.CompletionRequest{Model: "synthetic", Messages: []llm.Message{{Role: "user", Content: "synthetic"}}}
				_, err = provider.Complete(t.Context(), request)
				require.Error(t, err)
				_, err = provider.Complete(t.Context(), request)
				require.Error(t, err)
				require.EqualValues(t, 1, claims.Load())
				require.EqualValues(t, 1, sent.Load(), "SDK retries and compatibility fallback share the physical-attempt guard")
			})
		}
	}
}

func TestBoundedModelClientDisablesRedirectsAndTransportReplay(t *testing.T) {
	client := boundedModelClient(func(context.Context) error { return nil })
	request, err := http.NewRequest(http.MethodPost, "https://model.invalid/v1/responses", nil)
	require.NoError(t, err)
	require.ErrorIs(t, client.CheckRedirect(request, nil), http.ErrUseLastResponse)
	transport := singleAttemptTransport()
	require.True(t, transport.DisableKeepAlives)
	require.False(t, transport.ForceAttemptHTTP2)
	require.Empty(t, transport.TLSNextProto)
	require.Nil(t, transport.Proxy, "budgeted roles cannot inherit an unqualified ambient proxy")
}

func TestBudgetedLoopDoesNotRetryBlankContextOverflowOrTools(t *testing.T) {
	t.Setenv(contract.BoundedModelRequestsEnv, "1")
	t.Setenv(contract.ModelBudgetRunEnv, "run-uid")
	t.Setenv(contract.ModelBudgetRoleEnv, "checks")
	t.Setenv(contract.ModelBudgetDeadlineEnv, time.Now().Add(time.Minute).Format(time.RFC3339Nano))
	t.Setenv("ORKA_COORDINATION_ENABLED", "false")
	t.Setenv("ORKA_AUTONOMOUS_MODE", "false")
	for _, test := range []struct {
		name     string
		response *llm.CompletionResponse
		err      error
	}{
		{name: "blank", response: &llm.CompletionResponse{StopReason: "stop"}},
		{
			name: "tools", response: &llm.CompletionResponse{
				StopReason: "tool_calls", ToolCalls: []llm.ToolCall{{Name: "file_read"}},
			},
		},
		{name: "context", err: &llm.ProviderError{StatusCode: 400, Message: "context limit"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &mockProvider{response: test.response, err: test.err}
			_, err := executeAgentLoopWithEvents(t.Context(), provider, []llm.Message{{Role: "user", Content: "synthetic"}},
				"", "synthetic", modelSettings{}, nil, nil, nil, common.NoopEventRecorder{})
			require.Error(t, err)
			require.Len(t, provider.requests, 1)
			require.NotNil(t, provider.requests[0].Store)
			require.False(t, *provider.requests[0].Store)
		})
	}
}
