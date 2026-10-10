package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Catch a missing local bootstrap bound (including context-free lazy discovery):
// the normal model must run while optional Kubernetes endpoints remain stalled.
func TestNativeGatewayReplyStalledKubernetesDoesNotBlockModel(t *testing.T) {
	tools.RegisterBuiltinTools()
	for _, stalled := range []string{"discovery", "task", "origin"} {
		t.Run(stalled, func(t *testing.T) {
			logs := captureNativeReplyStderr(t)
			release := make(chan struct{})
			canceled := make(chan struct{}, 1)
			var discoveryCalls, originCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/tasks/task") {
					discoveryCalls.Add(1)
				}
				if (stalled == "discovery" && r.URL.Path == "/api") ||
					(stalled == "task" && strings.HasSuffix(r.URL.Path, "/tasks/task")) {
					select {
					case <-release:
					case <-r.Context().Done():
						canceled <- struct{}{}
					}
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				serveNativeReplyKubernetes(t, w, r)
			}))
			defer server.Close()
			env, tokenFile := nativeReplyBootstrapFixture(t)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originCalls.Add(1)
				if stalled == "origin" {
					select {
					case <-release:
					case <-r.Context().Done():
						canceled <- struct{}{}
					}
				}
				_, _ = fmt.Fprint(w, `{"taskUID":"uid"}`)
			}))
			defer origin.Close()
			env.ControllerURL = origin.URL
			newReader := func() (client.Reader, error) {
				return newNativeGatewayReplyTaskReader(&rest.Config{Host: server.URL})
			}
			type outcome struct {
				result     string
				err        error
				advertised bool
			}
			done := make(chan outcome, 1)
			go func() {
				sender, err := newNativeGatewayReplySender(t.Context(), newReader, env, tokenFile, 100*time.Millisecond)
				if err != nil {
					done <- outcome{err: err}
					return
				}
				tc := &tools.ToolContext{
					Namespace: env.TaskNamespace, TaskID: env.TaskName, TaskUID: env.TaskUID, GatewayReplySender: sender,
				}
				provider := &mockProvider{responses: []*llm.CompletionResponse{
					{Content: "normal final answer", StopReason: "end_turn"},
				}}
				result, err := executeAgentLoopWithEvents(
					t.Context(), provider, []llm.Message{{Role: "user", Content: "work"}}, "", "test-model",
					modelSettings{maxTokens: 4096}, buildLLMTools(env.Tools, nil, tc),
					nil, nil, common.NewFakeEventRecorder(), tc,
				)
				done <- outcome{result: result, err: err, advertised: len(buildLLMTools(env.Tools, nil, tc)) == 1}
			}()
			select {
			case got := <-done:
				if stalled != "discovery" {
					select {
					case <-canceled:
					case <-time.After(time.Second):
						t.Error("bootstrap returned without canceling its stalled HTTP request")
					}
				}
				close(release)
				require.NoError(t, got.err)
				require.Equal(t, "normal final answer", got.result)
				require.Equal(t, stalled == "discovery", got.advertised)
				require.Zero(t, discoveryCalls.Load(), "the production Task reader must not perform lazy discovery")
				if stalled == "task" {
					require.Zero(t, originCalls.Load(), "unread Task identity must never reach origin authentication")
				} else {
					require.Equal(t, int32(1), originCalls.Load())
				}
				if stalled == "discovery" {
					require.Empty(t, logs(), "successful bootstrap must not warn")
				} else {
					require.Equal(t, "warning: reply_in_conversation omitted reason=bootstrap_timeout\n", logs())
				}
			case <-time.After(500 * time.Millisecond):
				close(release)
				<-done // Join the test goroutine even on the pre-fix blocking path.
				t.Fatal("optional Kubernetes bootstrap blocked normal model work")
			}
		})
	}
}

// Catch silent availability omissions and accidental inclusion of opaque errors.
func TestNativeGatewayReplyAvailabilityWarningIsLocalAndSanitized(t *testing.T) {
	logs := captureNativeReplyStderr(t)
	env, tokenFile := nativeReplyBootstrapFixture(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("private-diagnostic https://private.invalid/task-text?token=opaque-token\nforged warning")
		},
	}).Build()
	sender, err := newNativeGatewayReplySender(
		t.Context(), func() (client.Reader, error) { return reader, nil }, env, tokenFile, time.Second,
	)
	require.NoError(t, err)
	require.Nil(t, sender)
	output := logs()
	require.Equal(t, 1, strings.Count(output, "warning:"), "omission must emit exactly one local warning")
	require.Contains(t, output, "reply_in_conversation")
	require.Contains(t, output, "reason=task_read_unavailable")
	for _, forbidden := range []string{"private-diagnostic", "private.invalid", "task-text", "opaque-token", "forged"} {
		require.NotContains(t, output, forbidden)
	}
	require.Less(t, len(output), 200)
}

// Catch treating a parent's cancellation/deadline as optional availability.
func TestNativeGatewayReplyParentCancellationPropagates(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			logs := captureNativeReplyStderr(t)
			env, tokenFile := nativeReplyBootstrapFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			wantErr := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				wantErr = context.DeadlineExceeded
			} else {
				cancel()
			}
			defer cancel()
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey,
					_ client.Object, _ ...client.GetOption) error {
					return ctx.Err()
				},
			}).Build()
			sender, err := newNativeGatewayReplySender(
				ctx, func() (client.Reader, error) { return reader, nil }, env, tokenFile, time.Second,
			)
			require.Nil(t, sender)
			require.ErrorIs(t, err, wantErr)
			require.Empty(t, logs())
		})
	}
}

// Catch resetting the local deadline between the Task and origin stages.
func TestNativeGatewayReplyBootstrapSharesOneBudget(t *testing.T) {
	logs := captureNativeReplyStderr(t)
	env, tokenFile := nativeReplyBootstrapFixture(t)
	var originCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/core.orka.ai/v1alpha1/namespaces/default/tasks/task":
			time.Sleep(150 * time.Millisecond)
			serveNativeReplyKubernetes(t, w, r)
		case "/internal/v1/tasks/default/task/gateway-messages/origin":
			originCalls.Add(1)
			select {
			case <-time.After(150 * time.Millisecond):
				_, _ = fmt.Fprint(w, `{"taskUID":"uid"}`)
			case <-r.Context().Done():
			}
		default:
			t.Errorf("unexpected bootstrap request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	env.ControllerURL = server.URL
	newReader := func() (client.Reader, error) {
		return newNativeGatewayReplyTaskReader(&rest.Config{Host: server.URL})
	}
	sender, err := newNativeGatewayReplySender(t.Context(), newReader, env, tokenFile, 250*time.Millisecond)
	require.NoError(t, err)
	require.Nil(t, sender, "two individually fast stages must not exceed the single optional budget")
	require.Equal(t, int32(1), originCalls.Load(), "the Task must succeed before the shared deadline expires")
	require.Equal(t, "warning: reply_in_conversation omitted reason=bootstrap_timeout\n", logs())
}

func TestNativeGatewayReplyPolicyExclusionSkipsBootstrap(t *testing.T) {
	for _, exclusion := range []string{"disabled", "not selected"} {
		t.Run(exclusion, func(t *testing.T) {
			logs := captureNativeReplyStderr(t)
			env, tokenFile := nativeReplyBootstrapFixture(t)
			if exclusion == "disabled" {
				env.GatewayReplyEnabled = false
			} else {
				env.Tools = []string{"web_search"}
			}
			newReader := func() (client.Reader, error) {
				t.Error("policy exclusion must not even construct the optional Kubernetes reader")
				return nil, errors.New("unavailable")
			}
			sender, err := newNativeGatewayReplySender(t.Context(), newReader, env, tokenFile, time.Second)
			require.NoError(t, err)
			require.Nil(t, sender)
			require.Empty(t, logs())
		})
	}
}

func TestNativeGatewayReplyReaderConstructionFailureOmitsTool(t *testing.T) {
	logs := captureNativeReplyStderr(t)
	env, tokenFile := nativeReplyBootstrapFixture(t)
	newReader := func() (client.Reader, error) {
		return nil, errors.New("private config URI/token diagnostics")
	}
	sender, err := newNativeGatewayReplySender(t.Context(), newReader, env, tokenFile, time.Second)
	require.NoError(t, err)
	require.Nil(t, sender)
	require.Equal(t, "warning: reply_in_conversation omitted reason=task_read_unavailable\n", logs())
}

// Catch downgrading definitive Kubernetes denials to optional availability, and
// leaking server-provided Warning headers or response bodies through the reader.
func TestNativeGatewayReplyKubernetesDenialsRemainFatal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			logs := captureNativeReplyStderr(t)
			env, tokenFile := nativeReplyBootstrapFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/apis/core.orka.ai/v1alpha1/namespaces/default/tasks/task" {
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Warning", `299 - "opaque upstream diagnostic"`)
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":%d,`+
					`"message":"private URI/token/Task text"}`, status)
			}))
			defer server.Close()
			newReader := func() (client.Reader, error) {
				return newNativeGatewayReplyTaskReader(&rest.Config{Host: server.URL})
			}
			sender, err := newNativeGatewayReplySender(t.Context(), newReader, env, tokenFile, time.Second)
			require.Nil(t, sender)
			require.EqualError(t, err, "gateway reply requires an authenticated native gateway Task")
			require.Empty(t, logs(), "explicit denials are fatal, not availability warnings")
		})
	}
}

func TestNativeGatewayReplyInFlightParentCancellation(t *testing.T) {
	for _, stage := range []string{"task", "origin", "origin deadline"} {
		t.Run(stage, func(t *testing.T) {
			logs := captureNativeReplyStderr(t)
			env, tokenFile := nativeReplyBootstrapFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			wantErr := context.Canceled
			if stage == "origin deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
				wantErr = context.DeadlineExceeded
			}
			defer cancel()
			canceled := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				isTask := strings.HasSuffix(r.URL.Path, "/tasks/task")
				if stage == "task" || !isTask {
					if stage != "origin deadline" {
						cancel()
					}
					<-r.Context().Done()
					canceled <- struct{}{}
					return
				}
				serveNativeReplyKubernetes(t, w, r)
			}))
			defer server.Close()
			env.ControllerURL = server.URL
			newReader := func() (client.Reader, error) {
				return newNativeGatewayReplyTaskReader(&rest.Config{Host: server.URL})
			}
			sender, err := newNativeGatewayReplySender(ctx, newReader, env, tokenFile, time.Second)
			require.Nil(t, sender)
			require.ErrorIs(t, err, wantErr)
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("parent cancellation did not reach the in-flight request")
			}
			require.Empty(t, logs())
		})
	}
}

func TestNativeGatewayReplyPrivateReaderDoesNotChangeSharedConfig(t *testing.T) {
	logs := captureNativeReplyStderr(t)
	env, tokenFile := nativeReplyBootstrapFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Warning", `299 - "opaque upstream URI/token/Task text"`)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, "opaque upstream body")
	}))
	defer server.Close()
	upstreamWarnings := rest.NewWarningWriter(io.Discard, rest.WarningWriterOptions{})
	config := &rest.Config{Host: server.URL, WarningHandler: upstreamWarnings}
	newReader := func() (client.Reader, error) { return newNativeGatewayReplyTaskReader(config) }
	sender, err := newNativeGatewayReplySender(t.Context(), newReader, env, tokenFile, time.Second)
	require.NoError(t, err)
	require.Nil(t, sender)
	require.Zero(t, upstreamWarnings.WarningCount(), "private reads must not emit opaque Kubernetes warnings")
	require.Equal(t, "warning: reply_in_conversation omitted reason=task_read_unavailable\n", logs())
	require.Same(t, upstreamWarnings, config.WarningHandler)
	require.Nil(t, config.WarningHandlerWithContext, "the caller's warning policy must remain unchanged")
	require.Zero(t, config.Timeout, "other tools must retain their existing transport behavior")
}

func nativeReplyBootstrapFixture(t *testing.T) (workerenv.AIWorkerEnv, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "projected-token")
	require.NoError(t, os.WriteFile(path, []byte("synthetic-projected-token"), 0600))
	return workerenv.AIWorkerEnv{
		BaseEnv: workerenv.BaseEnv{
			ControllerURL: "http://127.0.0.1:1", TaskNamespace: "default", TaskName: "task", TaskUID: "uid",
		},
		Tools: []string{"reply_in_conversation"}, GatewayReplyEnabled: true,
	}, path
}

func captureNativeReplyStderr(t *testing.T) func() string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	previous := os.Stderr
	os.Stderr = file
	t.Cleanup(func() {
		os.Stderr = previous
		require.NoError(t, file.Close())
	})
	return func() string {
		data, err := os.ReadFile(file.Name())
		require.NoError(t, err)
		return string(data)
	}
}

func serveNativeReplyKubernetes(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	var body string
	switch r.URL.Path {
	case "/api":
		body = `{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`
	case "/apis":
		body = `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"core.orka.ai",
			"versions":[{"groupVersion":"core.orka.ai/v1alpha1","version":"v1alpha1"}],
			"preferredVersion":{"groupVersion":"core.orka.ai/v1alpha1","version":"v1alpha1"}}]}`
	case "/apis/core.orka.ai/v1alpha1":
		body = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"core.orka.ai/v1alpha1",
			"resources":[{"name":"tasks","singularName":"task","namespaced":true,"kind":"Task","verbs":["get"]}]}`
	case "/apis/core.orka.ai/v1alpha1/namespaces/default/tasks/task":
		body = `{"apiVersion":"core.orka.ai/v1alpha1","kind":"Task",
			"metadata":{"namespace":"default","name":"task","uid":"uid"},"spec":{"type":"ai"}}`
	default:
		t.Errorf("unexpected Kubernetes request: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = fmt.Fprint(w, body)
}
