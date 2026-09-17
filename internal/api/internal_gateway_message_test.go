package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type gatewayMessageAPIFixture struct {
	h       *InternalHandlers
	app     *fiber.App
	service *gatewayruntime.Service
	db      *sqlite.Store
	task    *corev1alpha1.Task
	event   *store.GatewayEvent
	user    *UserInfo
	reviews int
	token   string
}

func newGatewayMessageAPIFixture(t *testing.T) *gatewayMessageAPIFixture {
	t.Helper()
	scheme := internalCallerAuthScheme(t)
	require.NoError(t, gatewayv1alpha1.AddToScheme(scheme))
	require.NoError(t, authenticationv1.AddToScheme(scheme))
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	f := &gatewayMessageAPIFixture{db: sqlite.NewStore(db, ":memory:"), token: fmt.Sprintf("gateway-message-test-%s-%d", t.Name(), time.Now().UnixNano())}
	g := &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default", UID: "gateway-uid", Generation: 1}, Spec: gatewayv1alpha1.GatewaySpec{GatewayClassName: "chat", Adapter: gatewayv1alpha1.GatewayAdapterLocation{Endpoint: "http://127.0.0.1:1"}, InboundAuthRef: gatewayv1alpha1.GatewayBearerAuthReference{Name: "inbound", Key: "token"}}, Status: gatewayv1alpha1.GatewayStatus{Ready: true, ObservedGeneration: 1, ObservedInboundAuthRefVersion: "1", ObservedCapabilities: &gatewayv1alpha1.GatewayObservedCapabilities{ContractVersion: protocol.Version, Capabilities: gatewayv1alpha1.GatewayCapabilities{InterimDelivery: true}}}}
	binding := &gatewayv1alpha1.GatewayBinding{ObjectMeta: metav1.ObjectMeta{Name: "room", Namespace: "default", UID: "binding-uid", Generation: 1}, Spec: gatewayv1alpha1.GatewayBindingSpec{GatewayRef: gatewayv1alpha1.GatewayBindingReference{Name: "chat"}, AgentRef: gatewayv1alpha1.GatewayBindingReference{Name: "assistant"}, Match: gatewayv1alpha1.GatewayBindingMatch{AccountID: "acct", ContextID: "room"}, SenderPolicy: gatewayv1alpha1.GatewaySenderPolicy{Mode: gatewayv1alpha1.GatewaySenderPolicyAllowlist, AllowedSenderIDs: []string{"user"}}, Session: gatewayv1alpha1.GatewaySessionSpec{Mode: gatewayv1alpha1.GatewaySessionContext}}, Status: gatewayv1alpha1.GatewayBindingStatus{Ready: true, ObservedGeneration: 1}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}, &gatewayv1alpha1.Gateway{}).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}}, g, binding,
		&gatewayv1alpha1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "chat", Generation: 1}, Spec: gatewayv1alpha1.GatewayClassSpec{ContractVersion: protocol.Version}, Status: gatewayv1alpha1.GatewayClassStatus{Accepted: true, ObservedGeneration: 1}},
		&corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "default", UID: "agent-uid"}, Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "test-model"}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "inbound", Namespace: "default", ResourceVersion: "1", Labels: map[string]string{gatewayruntime.GatewayInboundAuthLabel: gatewayruntime.GatewayAuthEnabledValue, gatewayruntime.GatewayAuthNameLabel: "chat"}, Annotations: map[string]string{gatewayruntime.GatewayAuthNameAnnotation: "chat"}}, Data: map[string][]byte{"token": []byte("inbound-token")}},
	).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if review, ok := obj.(*authenticationv1.TokenReview); ok {
			f.reviews++
			if review.Spec.Token == f.token {
				review.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: authenticationv1.UserInfo{Username: f.user.Username, UID: "worker-sa-uid", Extra: f.user.Extra}}
			}
			return nil
		}
		if task, ok := obj.(*corev1alpha1.Task); ok && task.UID == "" {
			task.UID = types.UID("task-uid")
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	f.service = gatewayruntime.NewService(kube, f.db, f.db, f.db, gatewayruntime.DefaultConfig())
	f.service.Config.AllowInsecureLoopback = true
	body, err := json.Marshal(protocol.EventEnvelope{ProtocolVersion: protocol.Version, ExternalEventID: "event", EventType: protocol.EventTypeText, AccountID: "acct", ContextID: "room", Sender: protocol.Sender{ID: "user"}, Text: "hello", ReplyTarget: "room"})
	require.NoError(t, err)
	admitted, err := f.service.AdmitEvent(t.Context(), "default", "chat", "Bearer inbound-token", body)
	require.NoError(t, err)
	require.NoError(t, f.service.DispatchOnce(t.Context()))
	f.event, err = f.db.GetGatewayEvent(t.Context(), "default", admitted.EventID)
	require.NoError(t, err)
	f.task = &corev1alpha1.Task{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: f.event.TaskName}, f.task))
	f.task.Status.Phase = corev1alpha1.TaskPhaseRunning
	f.task.Status.JobName = "job-a"
	f.task.Status.JobUID = "job-uid"
	require.NoError(t, kube.Status().Update(t.Context(), f.task))
	job := internalCallerAuthJob(f.task, "job-a", "job-uid")
	pod := internalCallerAuthPod(f.task, "pod-a", "pod-uid", job)
	require.NoError(t, kube.Create(t.Context(), job))
	require.NoError(t, kube.Create(t.Context(), pod))
	f.user = internalCallerAuthWorkerUser("pod-a", "pod-uid")
	f.h = NewInternalHandlers(f.db, f.db, f.db, f.db, f.db, InternalHandlersConfig{Client: kube, APIReader: kube, GatewayEventStore: f.db, GatewayService: f.service})
	f.app = fiber.New()
	f.app.Use(NewAuthMiddleware(kube))
	f.app.Post("/internal/v1/tasks/:namespace/:taskName/gateway-messages", f.h.SubmitGatewayMessage)
	return f
}

func (f *gatewayMessageAPIFixture) request(t *testing.T, body []byte) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/"+f.task.Name+"/gateway-messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.app.Test(req, fiber.TestConfig{Timeout: 15 * time.Second, FailOnTimeout: true})
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(data, &result), "status=%d body=%s", resp.StatusCode, data)
	return resp.StatusCode, result
}

func TestInternalGatewayMessageAuthenticWorkerReceipt(t *testing.T) {
	f := newGatewayMessageAPIFixture(t)
	status, receipt := f.request(t, []byte(`{"content":"working","requestID":"step-1"}`))
	require.Equal(t, 202, status)
	require.Equal(t, true, receipt["created"])
	require.Equal(t, "Pending", receipt["status"])
	require.Len(t, receipt, 3)
	require.Equal(t, 1, f.reviews)
	status, replay := f.request(t, []byte(`{"content":"working","requestID":"step-1"}`))
	require.Equal(t, 200, status)
	require.Equal(t, false, replay["created"])
	require.Equal(t, receipt["deliveryID"], replay["deliveryID"])
	status, failure := f.request(t, []byte(`{"content":"private changed content","requestID":"step-1"}`))
	require.Equal(t, 409, status)
	require.Equal(t, "conflict", failure["error"].(map[string]any)["code"])
	require.NotContains(t, fmt.Sprint(failure), "private changed content")
	rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: "default"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "working", rows[0].Text)
	task := &corev1alpha1.Task{}
	require.NoError(t, f.h.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(f.task), task))
	require.Equal(t, f.task, task)
	require.Empty(t, task.Spec.Prompt)
}

func TestInternalGatewayMessageUnsupportedCapabilityIsDistinct(t *testing.T) {
	for _, observed := range []string{"missing capabilities", "absent flag", "false flag"} {
		t.Run(observed, func(t *testing.T) {
			f := newGatewayMessageAPIFixture(t)
			object := &gatewayv1alpha1.Gateway{}
			require.NoError(t, f.h.k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "chat"}, object))
			switch observed {
			case "missing capabilities":
				object.Status.ObservedCapabilities = nil
			case "absent flag":
				object.Status.ObservedCapabilities = &gatewayv1alpha1.GatewayObservedCapabilities{ContractVersion: protocol.Version}
			case "false flag":
				object.Status.ObservedCapabilities.Capabilities.InterimDelivery = false
			}
			require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), object))
			status, result := f.request(t, []byte(`{"content":"private content","requestID":"step"}`))
			require.Equal(t, http.StatusConflict, status)
			require.Equal(t, map[string]any{"code": "interim_delivery_unsupported", "message": "gateway adapter does not advertise interimDelivery capability"}, result["error"])
			rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: "default"})
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

func TestInternalGatewayMessageRejectsUntrustedWorker(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *gatewayMessageAPIFixture)
	}{
		{"wrong pod UID", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.user.Extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"old-pod"}
		}},
		{"runtime SA", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.user.Username = "system:serviceaccount:default:runtime"
			f.user.Extra = nil
		}},
		{"controller SA", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.user.Username = "system:serviceaccount:default:controller"
			f.user.Extra = nil
		}},
		{"wrong Job UID", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Status.JobUID = "different-job"
			require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), f.task))
		}},
		{"wrong Task UID", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.UID = "replacement-task"
			require.NoError(t, f.h.k8sClient.Update(t.Context(), f.task))
		}},
		{"terminal", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), f.task))
		}},
		{"deleting", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Finalizers = []string{"test.orka.ai/hold"}
			require.NoError(t, f.h.k8sClient.Update(t.Context(), f.task))
			require.NoError(t, f.h.k8sClient.Delete(t.Context(), f.task))
		}},
		{"non gateway", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Spec.RequestedBy = nil
			require.NoError(t, f.h.k8sClient.Update(t.Context(), f.task))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newGatewayMessageAPIFixture(t)
			tt.change(t, f)
			status, result := f.request(t, []byte(`{"content":"private content","requestID":"step"}`))
			require.Equal(t, 403, status)
			require.Equal(t, "forbidden", result["error"].(map[string]any)["code"])
			require.NotContains(t, fmt.Sprint(result), "private content")
			require.Equal(t, 1, f.reviews)
			rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: "default"})
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

func TestInternalGatewayMessageStrictBoundedRequest(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
	}{
		{"target", `{"content":"x","requestID":"step","target":"room"}`, 400},
		{"policy", `{"content":"x","requestID":"step","maxMessages":100}`, 400},
		{"missing requestID", `{"content":"x"}`, 400},
		{"noncanonical requestID", `{"content":"x","requestID":" step "}`, 400},
		{"extra JSON", `{"content":"x","requestID":"step"}{}`, 400},
		{"raw invalid UTF8", "{\"content\":\"\xff\",\"requestID\":\"step\"}", 400},
		{"raw content cap", `{"content":"` + strings.Repeat("x", 16385) + `","requestID":"step"}`, 413},
		{"body cap", strings.Repeat(" ", 101000), 413},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newGatewayMessageAPIFixture(t)
			status, result := f.request(t, []byte(tt.body))
			require.Equal(t, tt.status, status)
			require.Contains(t, result, "error")
			rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: "default"})
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

func TestInternalGatewayMessageMasksErrorsWithoutCapabilitySentinel(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"matching domain text", &gatewayruntime.HTTPError{Code: http.StatusConflict, Message: gatewayruntime.ErrInterimDeliveryUnsupported.Error()}, http.StatusConflict, "conflict"},
		{"matching framework text", fiber.NewError(http.StatusConflict, gatewayruntime.ErrInterimDeliveryUnsupported.Error()), http.StatusConflict, "conflict"},
		{"private store diagnostic", fmt.Errorf("private store diagnostic: %s", gatewayruntime.ErrInterimDeliveryUnsupported.Error()), http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()
			app.Post("/message", func(c fiber.Ctx) error { return sendGatewayMessageError(c, tt.err) })
			response, err := app.Test(httptest.NewRequest(http.MethodPost, "/message", nil))
			require.NoError(t, err)
			defer response.Body.Close() //nolint:errcheck
			require.Equal(t, tt.status, response.StatusCode)
			var result struct {
				Error struct{ Code, Message string }
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
			require.Equal(t, tt.code, result.Error.Code)
			require.NotContains(t, result.Error.Message, "private store diagnostic")
			require.NotContains(t, result.Error.Message, "interimDelivery")
		})
	}
}

func TestInternalGatewayMessageStreamReadIsBounded(t *testing.T) {
	app := fiber.New()
	app.Post("/message", func(c fiber.Ctx) error {
		reader := &gatewayMessageCountingReader{Reader: strings.NewReader(strings.Repeat("x", 2*maxGatewayMessageRequestBytes))}
		c.Request().SetBodyStream(reader, -1)
		_, err := readGatewayMessageRequest(c)
		require.Error(t, err)
		require.LessOrEqual(t, reader.read, maxGatewayMessageRequestBytes+1, "must not materialize the entire stream before checking the limit")
		return sendGatewayMessageError(c, err)
	})
	response, err := app.Test(httptest.NewRequest(http.MethodPost, "/message", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

type gatewayMessageCountingReader struct {
	io.Reader
	read int
}

func (r *gatewayMessageCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func TestInternalGatewayMessageChecksRevocationInsideWriter(t *testing.T) {
	f := newGatewayMessageAPIFixture(t)
	// Register the Task's cleanup fence first so the test revokes only during the
	// admission authorization, after the worker has been authenticated.
	require.NoError(t, f.db.WithAuthorizedTaskDataTransaction(t.Context(), "default", f.task.Name, func(context.Context) error { return nil }, func(context.Context) error { return nil }))
	revoked := false
	fresh := interceptor.NewClient(f.h.k8sClient.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		err := c.Get(ctx, key, obj, opts...)
		if _, ok := obj.(*gatewayv1alpha1.Gateway); ok && !revoked {
			// This real write would deadlock if a Kubernetes read ran inside the writer.
			require.NoError(t, f.db.RevokeTaskJob(ctx, store.TaskJobIdentity{Namespace: "default", TaskUID: string(f.task.UID), JobUID: f.task.Status.JobUID}))
			revoked = true
		}
		return err
	}})
	f.h.apiReader = fresh
	f.service.APIReader = fresh
	status, result := f.request(t, []byte(`{"content":"working","requestID":"step"}`))
	require.True(t, revoked)
	require.Equal(t, 403, status)
	require.Contains(t, result, "error")
	rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: "default"})
	require.NoError(t, err)
	require.Empty(t, rows)
}
