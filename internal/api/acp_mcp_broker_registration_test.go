package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/workers/acp/supervisor"
)

func TestRegisterACPMCPBrokerValidatesDependenciesAndInstallsRoute(t *testing.T) {
	app := fiber.New()
	if err := RegisterACPMCPBroker(app, &controller.ACPMCPBroker{}); err == nil {
		t.Fatal("invalid broker registration unexpectedly succeeded")
	}
	broker := &controller.ACPMCPBroker{
		Credentials: controller.ACPMCPBrokerCredentialResolverFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) (controller.ACPMCPBrokerCredentials, error) {
			return controller.ACPMCPBrokerCredentials{}, store.ErrNotFound
		}),
		Prompts: controller.ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error { return nil }),
		Executor: controller.ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		}),
		Effects: inertExternalEffectStore{},
	}
	if err := RegisterACPMCPBroker(app, broker); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, harnessv2.MCPBrokerCallPath, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusNotFound {
		t.Fatal("registered MCP broker route returned 404")
	}
}

type inertExternalEffectStore struct{}

func (inertExternalEffectStore) ReserveExternalEffect(context.Context, store.ReserveExternalEffectRequest) (*store.ExternalEffect, error) {
	return nil, store.ErrNotFound
}
func (inertExternalEffectStore) GetExternalEffect(context.Context, string) (*store.ExternalEffect, error) {
	return nil, store.ErrNotFound
}
func (inertExternalEffectStore) GetExternalEffectByIdentity(context.Context, store.ExternalEffectIdentity) (*store.ExternalEffect, error) {
	return nil, store.ErrNotFound
}
func (inertExternalEffectStore) TransitionExternalEffect(context.Context, store.ExternalEffectTransition) (*store.ExternalEffect, error) {
	return nil, store.ErrNotFound
}

// handlerScopedRequestContext stands in for the pooled *fasthttp.RequestCtx
// that fasthttpadaptor passes as r.Context(). Like that context it is neither a
// WithCancel context nor an AfterFunc implementer, so a cancelable context
// derived from it starts a standard-library goroutine that calls Done whenever
// it is scheduled. It records every use after the broker takes the request.
type handlerScopedRequestContext struct {
	done      chan struct{}
	handedOff atomic.Bool
	lateUses  atomic.Int32
}

func (c *handlerScopedRequestContext) use() {
	if c.handedOff.Load() {
		c.lateUses.Add(1)
	}
}

func (c *handlerScopedRequestContext) Deadline() (time.Time, bool) {
	c.use()
	return time.Time{}, false
}

func (c *handlerScopedRequestContext) Done() <-chan struct{} {
	c.use()
	return c.done
}

func (c *handlerScopedRequestContext) Err() error {
	c.use()
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c *handlerScopedRequestContext) Value(any) any {
	c.use()
	return nil
}

type brokerProbeTool struct{}

func (brokerProbeTool) Name() string                { return "probe_tool" }
func (brokerProbeTool) Description() string         { return "Returns a fixed result." }
func (brokerProbeTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (brokerProbeTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "", nil
}

func TestACPMCPBrokerRouteRunsOnHandlerOwnedContext(t *testing.T) {
	for _, serverShutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("serverShutdown=%t", serverShutdown), func(t *testing.T) {
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"}}
			request, profile := newACPMCPTracingRequest(t, task, brokerProbeTool{})
			request.Call.Arguments = json.RawMessage(`{}`)
			effects := newSQLiteExecutionEventStoreForTest(t)
			epoch, err := effects.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
				NewEpoch: 1, HolderID: "controller", RequestDigest: store.CanonicalBytesDigest([]byte("handler-context-epoch")),
			})
			require.NoError(t, err)
			frozenPolicy := request.Authorization.Configuration()
			credentials := controller.ACPMCPBrokerCredentials{
				// These credentials are synthetic and never leave the loopback test server.
				ControllerBearerToken: strings.Repeat("b", 32), CapabilitySecret: bytes.Repeat([]byte("c"), harnessv2.MinCapabilitySecretBytes),
				ExpectedFence: request.Metadata.Fence, RuntimeProfile: profile, ExpectedMCPConfiguration: &frozenPolicy,
				ControllerFence: store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID},
				Task:            controller.ACPMCPAuthenticatedTask{Name: task.Name, Namespace: task.Namespace, UID: string(task.UID)},
			}
			toolStarted := make(chan struct{})
			toolErr := make(chan error, 1)
			broker := &controller.ACPMCPBroker{
				Credentials: controller.ACPMCPBrokerCredentialResolverFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) (controller.ACPMCPBrokerCredentials, error) {
					return credentials, nil
				}),
				Prompts: controller.ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error { return nil }),
				Effects: effects,
				Executor: controller.ACPMCPToolExecutorFunc(func(ctx context.Context, _ harnessv2.MCPBrokerCallRequest, _ harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
					if !serverShutdown {
						return json.RawMessage(`{"ok":true}`), nil
					}
					close(toolStarted)
					<-ctx.Done()
					toolErr <- ctx.Err()
					return nil, ctx.Err()
				}),
			}
			requestCtx := &handlerScopedRequestContext{done: make(chan struct{})}
			brokerHandler := acpMCPBrokerHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCtx.handedOff.Store(true)
				broker.ServeHTTP(w, r)
			}))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				brokerHandler.ServeHTTP(w, r.WithContext(requestCtx))
			}))
			defer server.Close()
			client, err := supervisor.NewControllerMCPBrokerClient(server.URL, task.Namespace, credentials.ControllerBearerToken, credentials.CapabilitySecret)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if serverShutdown {
				// Closing the request context's Done channel is how fasthttp
				// signals server shutdown to a running handler.
				go func() {
					select {
					case <-toolStarted:
						close(requestCtx.done)
					case <-ctx.Done():
					}
				}()
			}
			response, err := client.Call(ctx, request)
			// Close waits for the handler to return.
			server.Close()
			if serverShutdown {
				require.Error(t, err)
				select {
				case err := <-toolErr:
					require.ErrorIs(t, err, context.Canceled, "server shutdown must cancel the running tool")
				default:
					t.Fatal("the broker did not run the tool")
				}
			} else {
				require.NoError(t, err)
				require.False(t, response.IsError)
			}
			require.Zero(t, requestCtx.lateUses.Load(), "the broker used the pooled request context after the adapter handed off the request")
		})
	}
}
