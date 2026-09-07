package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestACPDispatcherPermissionResolutionRetriesUnsentPreflight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newPermissionResolutionRetryFixture()
		started := time.Now().UTC()
		want := fixture.request(t, started.Add(30*time.Second))
		preflights, writes := 0, 0
		runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
			preflights++
			if preflights < 3 {
				time.Sleep(250 * time.Millisecond)
				return harnessv2.MarkPreMutationRetryable(errors.New("synthetic authority lookup unavailable"))
			}
			return nil
		}, func(request *http.Request) (*http.Response, error) {
			writes++
			got := readPermissionResolutionRetryRequest(t, request)
			if !reflect.DeepEqual(got, want) {
				t.Fatal("permission retry changed the original sealed request, fence, decision, or expiry")
			}
			return permissionResolutionRetryResponse(t, got), nil
		})

		if err := fixture.resolve(context.Background(), runtimeClient); err != nil {
			t.Fatalf("resolve permission after definitely-unsent preflight failures: %v", err)
		}
		if preflights != 3 || writes != 1 {
			t.Fatalf("preflights = %d, mutation writes = %d; want 3 and 1", preflights, writes)
		}
		if elapsed := time.Since(started); elapsed < 500*time.Millisecond || elapsed >= 30*time.Second {
			t.Fatalf("retry elapsed = %s; want delayed success within original expiry", elapsed)
		}
	})
}

func TestACPDispatcherPermissionResolutionDoesNotReplayUncertainWrite(t *testing.T) {
	for _, readBody := range []bool{false, true} {
		name := "custom transport with unknown write"
		if readBody {
			name = "response lost after request body consumed"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newPermissionResolutionRetryFixture()
				preflights, writes := 0, 0
				runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
					preflights++
					return nil
				}, func(request *http.Request) (*http.Response, error) {
					writes++
					if readBody {
						_ = readPermissionResolutionRetryRequest(t, request)
					}
					return nil, io.ErrUnexpectedEOF
				})

				err := fixture.resolve(context.Background(), runtimeClient)
				var clientErr *harnessv2.ClientError
				if !errors.As(err, &clientErr) || clientErr.Kind != harnessv2.ClientErrorTransport || clientErr.WriteEvidence.SafeToResendSameIdentity() {
					t.Fatalf("error = %v; want transport failure without zero-write proof", err)
				}
				if readBody && clientErr.WriteEvidence.RequestBodyBytesRead == 0 {
					t.Fatal("missing evidence that the original request body was consumed")
				}
				if preflights != 1 || writes != 1 {
					t.Fatalf("preflights = %d, mutation writes = %d; uncertain mutation must not replay", preflights, writes)
				}
			})
		})
	}
}

func TestACPDispatcherPermissionResolutionRejectsDefinitivePreflight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newPermissionResolutionRetryFixture()
		preflights, writes := 0, 0
		runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
			preflights++
			return errors.New("synthetic frozen authority changed")
		}, func(*http.Request) (*http.Response, error) {
			writes++
			return nil, errors.New("rejected permission must not reach transport")
		})

		err := fixture.resolve(context.Background(), runtimeClient)
		var clientErr *harnessv2.ClientError
		if !errors.As(err, &clientErr) || clientErr.Retryable || !clientErr.WriteEvidence.SafeToResendSameIdentity() {
			t.Fatalf("error = %v; want definitive zero-write validation rejection", err)
		}
		if preflights != 1 || writes != 0 {
			t.Fatalf("preflights = %d, mutation writes = %d; want 1 and 0", preflights, writes)
		}
	})
}

func TestACPDispatcherPermissionResolutionRetryBounds(t *testing.T) {
	for _, test := range []struct {
		name           string
		callerDeadline time.Duration
		preflightDelay time.Duration
		wantElapsed    time.Duration
	}{
		{name: "caller deadline during backoff", callerDeadline: time.Millisecond, wantElapsed: time.Millisecond},
		{name: "original expiry during backoff", preflightDelay: 30*time.Second - time.Millisecond, wantElapsed: 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newPermissionResolutionRetryFixture()
				started := time.Now()
				ctx := context.Background()
				if test.callerDeadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, test.callerDeadline)
					defer cancel()
				}
				preflights, writes := 0, 0
				runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
					preflights++
					// Deliver a known-unsent result close to expiry without giving
					// the retry loop a new operation or a fresh validity window.
					time.Sleep(test.preflightDelay)
					return harnessv2.MarkPreMutationRetryable(errors.New("synthetic authority lookup unavailable"))
				}, func(*http.Request) (*http.Response, error) {
					writes++
					return nil, errors.New("expired permission must not reach transport")
				})

				if err := fixture.resolve(ctx, runtimeClient); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("resolve permission = %v; want deadline exceeded", err)
				}
				if preflights != 1 || writes != 0 {
					t.Fatalf("preflights = %d, mutation writes = %d; want 1 and 0", preflights, writes)
				}
				if elapsed := time.Since(started); elapsed != test.wantElapsed {
					t.Fatalf("retry elapsed = %s, want %s", elapsed, test.wantElapsed)
				}
			})
		})
	}
}

func TestACPDispatcherPermissionResolutionReturnsLastUnsentError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newPermissionResolutionRetryFixture()
		preflights, writes := 0, 0
		runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
			preflights++
			return harnessv2.MarkPreMutationRetryable(errors.New("synthetic authority lookup unavailable"))
		}, func(*http.Request) (*http.Response, error) {
			writes++
			return nil, errors.New("unvalidated permission must not reach transport")
		})

		err := fixture.resolve(context.Background(), runtimeClient)
		if !retryableUnsentMutationCanRetry(err) {
			t.Fatalf("retry exhaustion lost original definitely-unsent classification: %v", err)
		}
		if preflights != retry.DefaultBackoff.Steps || writes != 0 {
			t.Fatalf("preflights = %d, mutation writes = %d; want bounded backoff exhaustion and zero writes", preflights, writes)
		}
	})
}

type permissionResolutionRetryFixture struct {
	task   *corev1alpha1.Task
	fence  harnessv2.Fence
	policy harnessv2.MCPPolicyConfiguration
	event  harnessv2.Event
}

func newPermissionResolutionRetryFixture() permissionResolutionRetryFixture {
	return permissionResolutionRetryFixture{
		task: &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "permission-task", UID: "permission-task-uid"},
			Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
				Attempt: 2, PromptID: "permission-prompt",
			}},
		},
		fence: harnessv2.Fence{
			RuntimeInstanceID: "permission-instance", SupervisorBootID: "permission-boot", ControllerEpoch: 7,
			RuntimePoolUID: "permission-pool", RuntimePoolGeneration: 3,
			RuntimeSessionUID: "permission-session", RuntimeSessionGeneration: 4,
			RuntimeProfileDigest:       harnessv2.ProfileDigest("sha256:" + strings.Repeat("a", 64)),
			ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
		},
		policy: harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{
			AllowedToolNames: []string{"native-read"},
			Tools: []harnessv2.MCPToolDescriptor{{
				Name: "native-read", Source: harnessv2.MCPToolSourceProviderNative, Effect: harnessv2.MCPToolEffectReadOnly,
			}},
		}},
		event: harnessv2.Event{PermissionRequested: &harnessv2.PermissionRequestedEvent{
			RequestID: "permission-request", ToolName: "native-read",
			Options: []harnessv2.PermissionOption{
				{OptionID: "allow-once", Kind: harnessv2.PermissionOptionAllowOnce},
				{OptionID: "reject-once", Kind: harnessv2.PermissionOptionRejectOnce},
			},
		}},
	}
}

func (f permissionResolutionRetryFixture) resolve(ctx context.Context, runtimeClient *harnessv2.Client) error {
	return (&ACPDispatcher{}).resolvePromptPermission(ctx, runtimeClient, "permission-runtime-session", f.task, f.fence, f.policy, f.event)
}

func (f permissionResolutionRetryFixture) request(t *testing.T, expiresAt time.Time) harnessv2.ResolvePermissionRequest {
	t.Helper()
	request := harnessv2.ResolvePermissionRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: harnessv2.MutationMetadata{
			Fence: f.fence, TaskUID: harnessv2.TaskUID(f.task.UID), TaskAttempt: 2, PromptID: "permission-prompt",
			OperationID: "permission-permission-request-permission-prompt", ExpiresAt: expiresAt,
			RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion,
		},
		RequestID: "permission-request", Decision: harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: "allow-once"},
	}
	if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
		t.Fatal(err)
	}
	return request
}

type permissionResolutionRetryTransport func(*http.Request) (*http.Response, error)

func (f permissionResolutionRetryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func permissionResolutionRetryClient(t *testing.T, beforeMutation func(context.Context, string) error, transport permissionResolutionRetryTransport) *harnessv2.Client {
	t.Helper()
	runtimeClient, err := harnessv2.NewClient("http://permission-runtime.invalid",
		harnessv2.WithHTTPClient(&http.Client{Transport: transport}),
		harnessv2.WithControllerBearerToken(strings.Repeat("c", 32)),
		harnessv2.WithOperationCapabilitySecret([]byte(strings.Repeat("s", 32))),
		harnessv2.WithBeforeMutation(beforeMutation),
	)
	if err != nil {
		t.Fatal(err)
	}
	return runtimeClient
}

func readPermissionResolutionRetryRequest(t *testing.T, request *http.Request) harnessv2.ResolvePermissionRequest {
	t.Helper()
	if request.Method != http.MethodPut || request.URL.Path != "/v2/runtime-sessions/permission-runtime-session/prompts/permission-prompt/permissions/permission-request" {
		t.Fatal("permission retry changed its mutation method or target")
	}
	var decoded harnessv2.ResolvePermissionRequest
	if err := json.NewDecoder(request.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateAt(time.Now().UTC()); err != nil {
		t.Fatalf("permission mutation is not valid within its original expiry: %v", err)
	}
	return decoded
}

func permissionResolutionRetryResponse(t *testing.T, request harnessv2.ResolvePermissionRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(harnessv2.PermissionResolutionResponse{
		Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
		State: harnessv2.PermissionResolutionApplied, Decision: request.Decision, ResolvedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}
}
