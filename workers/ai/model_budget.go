package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/workerenv"
)

var errModelCallBudget = errors.New("model request budget exhausted or unavailable")

func boundedModelRequests(getenv func(string) string) (bool, error) {
	value := getenv(contract.BoundedModelRequestsEnv)
	if value == "" {
		if getenv(contract.ModelBudgetRunEnv) != "" || getenv(contract.ModelBudgetRoleEnv) != "" ||
			getenv(contract.ModelBudgetDeadlineEnv) != "" {
			return false, errModelCallBudget
		}
		return false, nil
	}
	if value != "1" || getenv(contract.ModelBudgetRunEnv) == "" || getenv(contract.ModelBudgetRoleEnv) == "" {
		return false, errModelCallBudget
	}
	if _, err := time.Parse(time.RFC3339Nano, getenv(contract.ModelBudgetDeadlineEnv)); err != nil {
		return false, errModelCallBudget
	}
	return true, nil
}

func validateBoundedModelEnvironment(getenv func(string) string, workerEnv workerenv.AIWorkerEnv) error {
	coordination := workerenv.ParseCoordinationEnv(getenv)
	if len(workerEnv.Tools) != 0 || len(workerEnv.Fallbacks) != 0 ||
		coordination.Enabled || coordination.AutonomousMode || getenv(workerenv.SessionName) != "" ||
		getenv(workerenv.MemoryContextEnabled) != "false" || getenv(workerenv.MemoryToolsAutoEnable) != "false" ||
		workerEnv.TaskName == "" || workerEnv.TaskNamespace == "" || workerEnv.TaskUID == "" {
		return errors.New("budgeted model requests require isolated native roles " +
			"without tools, memory, sessions, coordination, or fallbacks")
	}
	return nil
}

type modelBudgetTransport struct {
	used  atomic.Bool
	claim func(context.Context) error
	send  func(*http.Request) (*http.Response, error)
}

func (t *modelBudgetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.Method != http.MethodPost || t.claim == nil || t.send == nil ||
		request.Context().Err() != nil || !t.used.CompareAndSwap(false, true) {
		return nil, errModelCallBudget
	}
	if err := t.claim(request.Context()); err != nil {
		return nil, errModelCallBudget
	}
	return t.send(request)
}

// A fresh HTTP/1 connection cannot silently replay a POST on a stale pooled
// connection. Redirects, SDK retries, API probing fallbacks and loop retries
// all encounter the same one-shot transport before another outbound attempt.
func singleAttemptTransport() *http.Transport {
	transport := new(http.Transport)
	if standard, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = standard.Clone()
	}
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	transport.Proxy = nil
	return transport
}

func boundedModelClient(claim func(context.Context) error) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &modelBudgetTransport{
			claim: claim,
			send: func(request *http.Request) (*http.Response, error) {
				transport := singleAttemptTransport()
				defer transport.CloseIdleConnections()
				return transport.RoundTrip(request)
			},
		},
	}
}

func modelCallPermitFromEnvironment(getenv func(string) string) (func(context.Context) error, error) {
	controller, err := url.Parse(getenv(workerenv.ControllerURL))
	if err != nil || controller.Host == "" || controller.User != nil || controller.RawQuery != "" ||
		controller.Fragment != "" || (controller.Scheme != "http" && controller.Scheme != "https") {
		return nil, errModelCallBudget
	}
	namespace, taskName := getenv(workerenv.TaskNamespace), getenv(workerenv.TaskName)
	if namespace == "" || taskName == "" || strings.ContainsAny(namespace+taskName, "/?#") {
		return nil, errModelCallBudget
	}
	controller.Path = strings.TrimRight(controller.Path, "/") + "/internal/v1/tasks/" +
		url.PathEscape(namespace) + "/" + url.PathEscape(taskName) + "/remediation-model-call"
	endpoint := controller.String()
	tokenPath := getenv(workerenv.ServiceAccountTokenPath)
	if tokenPath == "" {
		tokenPath = workerenv.ServiceAccountTokenFile
	}
	return func(ctx context.Context) error {
		credential, err := os.ReadFile(tokenPath)
		if err != nil || len(credential) == 0 {
			return errModelCallBudget
		}
		transport := singleAttemptTransport()
		defer transport.CloseIdleConnections()
		client := &http.Client{
			Transport: transport, Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		for attempt := range 5 {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
			if err != nil {
				return errModelCallBudget
			}
			request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(credential)))
			response, err := client.Do(request)
			if err != nil {
				return errModelCallBudget
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusNoContent {
				return nil
			}
			if response.StatusCode != http.StatusServiceUnavailable || attempt == 4 {
				return errModelCallBudget
			}
			// A fast worker may precede the controller's Job-UID status write.
			// Retrying a permit never replays a model request; consumed permits
			// are not re-granted, even after an uncertain API acknowledgement.
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errModelCallBudget
			case <-timer.C:
			}
		}
		return errModelCallBudget
	}, nil
}
