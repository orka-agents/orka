package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/workerclient"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func originRequest(t *testing.T, f *gatewayMessageAPIFixture, wantStatus int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/tasks/default/"+f.task.Name+"/gateway-messages/origin", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, wantStatus, resp.StatusCode)
	if wantStatus == 200 {
		var body map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		require.Equal(t, map[string]any{"taskUID": string(f.task.UID)}, body, "origin contains no routing, text, budget or admission grant")
	}
}

func TestInternalGatewayReplyOriginAdmissionWithdrawalAndRecovery(t *testing.T) {
	for _, readiness := range []bool{true, false} {
		t.Run(map[bool]string{true: "readiness", false: "capability"}[readiness], func(t *testing.T) {
			f := newGatewayMessageAPIFixture(t)
			eligible, err := f.service.ResolveReplyEligibility(t.Context(), f.task)
			require.NoError(t, err)
			require.True(t, eligible, "planning before withdrawal")
			status, receipt := f.request(t, []byte(`{"content":"working","requestID":"existing"}`))
			require.Equal(t, 202, status)
			g := &gatewayv1alpha1.Gateway{}
			require.NoError(t, f.h.k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "chat"}, g))
			if readiness {
				g.Status.Ready = false
			} else {
				g.Status.ObservedCapabilities.Capabilities.InterimDelivery = false
			}
			require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), g))
			originRequest(t, f, 200)
			status, _ = budgetRequest(t, f, "new")
			wantStatus := 409
			if readiness {
				wantStatus = 503
			}
			require.Equal(t, wantStatus, status)
			status, _ = f.request(t, []byte(`{"content":"working","requestID":"new"}`))
			require.Equal(t, wantStatus, status)
			status, _ = budgetRequest(t, f, "existing")
			require.Equal(t, 200, status)
			status, replay := f.request(t, []byte(`{"content":"working","requestID":"existing"}`))
			require.Equal(t, 200, status)
			require.Equal(t, receipt["deliveryID"], replay["deliveryID"])
			g.Status.Ready = true
			g.Status.ObservedCapabilities.Capabilities.InterimDelivery = true
			require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), g))
			originRequest(t, f, 200)
			status, _ = budgetRequest(t, f, "new")
			require.Equal(t, 200, status)
			status, _ = f.request(t, []byte(`{"content":"working","requestID":"new"}`))
			require.Equal(t, 202, status)
		})
	}
}

func TestInternalGatewayReplyOriginRejectsInvalidIdentities(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *gatewayMessageAPIFixture)
		status int
	}{
		{"forged Pod", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.user.Extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"forged"}
		}, 403},
		{"forged origin", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Spec.RequestedBy = nil
			require.NoError(t, f.h.k8sClient.Update(t.Context(), f.task))
		}, 403},
		{"replaced Task", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.UID = "replaced"
			require.NoError(t, f.h.k8sClient.Update(t.Context(), f.task))
		}, 403},
		{"replaced Gateway", func(t *testing.T, f *gatewayMessageAPIFixture) {
			g := &gatewayv1alpha1.Gateway{}
			require.NoError(t, f.h.k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "chat"}, g))
			g.UID = "replaced"
			require.NoError(t, f.h.k8sClient.Update(t.Context(), g))
		}, 409},
		{"replaced namespace", func(t *testing.T, f *gatewayMessageAPIFixture) {
			ns := &corev1.Namespace{}
			require.NoError(t, f.h.k8sClient.Get(t.Context(), client.ObjectKey{Name: "default"}, ns))
			ns.UID = "replaced"
			require.NoError(t, f.h.k8sClient.Update(t.Context(), ns))
		}, 409},
		{"revoked Job", func(t *testing.T, f *gatewayMessageAPIFixture) {
			require.NoError(t, f.db.RevokeTaskJob(t.Context(), store.TaskJobIdentity{Namespace: f.task.Namespace, TaskUID: string(f.task.UID), JobUID: f.task.Status.JobUID}))
		}, 403},
		{"container", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Spec.Type = corev1alpha1.TaskTypeContainer
			require.NoError(t, f.h.k8sClient.Update(t.Context(), f.task))
		}, 403},
		{"ACP", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Spec.Type = corev1alpha1.TaskTypeAgent
			require.NoError(t, f.h.k8sClient.Update(t.Context(), f.task))
		}, 403},
		{"delegated", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Labels[labels.LabelParentTask] = "parent"
			require.NoError(t, f.h.k8sClient.Update(t.Context(), f.task))
		}, 403},
		{"closed event", func(t *testing.T, f *gatewayMessageAPIFixture) {
			require.NoError(t, f.db.ExpireGatewayEvent(t.Context(), f.event.Namespace, f.event.ID, "", "closed", time.Now()))
		}, 403},
		{"terminal", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), f.task))
		}, 403},
		{"Finalizing", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Status.Phase = corev1alpha1.TaskPhaseFinalizing
			require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), f.task))
		}, 403},
		{"publication pending", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Status.JobName = ""
			f.task.Status.JobUID = ""
			require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), f.task))
		}, 503},
		{"nil service", func(t *testing.T, f *gatewayMessageAPIFixture) { f.h.gatewayService = nil }, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newGatewayMessageAPIFixture(t)
			test.mutate(t, f)
			originRequest(t, f, test.status)
			rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: "default"})
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

func TestInternalGatewayReplyOriginClientWaitsForJobPublication(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "revoked before retry"}[revoke], func(t *testing.T) {
			f := newGatewayMessageAPIFixture(t)
			jobName, jobUID := f.task.Status.JobName, f.task.Status.JobUID
			f.task.Status.JobName, f.task.Status.JobUID = "", ""
			require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), f.task))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				response, err := f.app.Test(r)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				defer func() { require.NoError(t, response.Body.Close()) }()
				if requests.Add(1) == 1 {
					require.Equal(t, 503, response.StatusCode, "real native authorization must deny before Job publication")
					current := f.task.DeepCopy()
					current.Status.JobName, current.Status.JobUID = jobName, jobUID
					require.NoError(t, f.h.k8sClient.Status().Update(t.Context(), current))
					if revoke {
						require.NoError(t, f.db.RevokeTaskJob(t.Context(), store.TaskJobIdentity{Namespace: current.Namespace, TaskUID: string(current.UID), JobUID: jobUID}))
					}
				}
				w.WriteHeader(response.StatusCode)
				_, _ = io.Copy(w, response.Body)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(path, []byte(f.token), 0600))
			sender, err := workerclient.New(workerclient.Config{ControllerURL: server.URL, Namespace: f.task.Namespace, TaskName: f.task.Name, TaskUID: string(f.task.UID), TokenFile: path})
			require.NoError(t, err)
			err = sender.AuthenticateOrigin(t.Context())
			if revoke {
				require.Error(t, err)
				require.NotErrorIs(t, err, workerclient.ErrUnavailable)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, int32(2), requests.Load())
		})
	}
}

func TestInternalGatewayReplyOriginWriterClosure(t *testing.T) {
	f := newGatewayMessageAPIFixture(t)
	closed := false
	fresh := interceptor.NewClient(f.h.k8sClient.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		err := c.Get(ctx, key, obj, opts...)
		if _, ok := obj.(*gatewayv1alpha1.Gateway); ok && !closed {
			require.NoError(t, f.db.ExpireGatewayEvent(ctx, f.event.Namespace, f.event.ID, "", "closed", time.Now()))
			closed = true
		}
		return err
	}})
	f.service.APIReader = fresh
	originRequest(t, f, 403)
	require.True(t, closed)
}

func TestInternalGatewayReplyOriginServerRouteKeepsCallerAuthority(t *testing.T) {
	for _, identity := range []string{"valid", "forged", "revoked"} {
		t.Run(identity, func(t *testing.T) {
			f := newGatewayMessageAPIFixture(t)
			status := 403
			switch identity {
			case "valid":
				status = 200
			case "forged":
				f.user.Extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"forged"}
			case "revoked":
				require.NoError(t, f.db.RevokeTaskJob(t.Context(), store.TaskJobIdentity{Namespace: f.task.Namespace, TaskUID: string(f.task.UID), JobUID: f.task.Status.JobUID}))
			}
			server := NewServer(f.h.k8sClient, nil, ServerConfig{GatewayService: f.service, ResultStore: f.db})
			f.app = server.app
			originRequest(t, f, status)
		})
	}
}

func TestInternalGatewayReplyOriginInvalidTokenStillUnauthorized(t *testing.T) {
	f := newGatewayMessageAPIFixture(t)
	var reviews atomic.Int32
	kube := interceptor.NewClient(f.h.k8sClient.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if review, ok := obj.(*authenticationv1.TokenReview); ok {
				reviews.Add(1)
				review.Status.Authenticated = false
				return nil
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	server := NewServer(kube, nil, ServerConfig{GatewayService: f.service, ResultStore: f.db})
	f.app = server.app
	originRequest(t, f, 401)
	require.Equal(t, int32(1), reviews.Load())
	_, cached := tokenCache.Load(getTokenHash(f.token))
	require.False(t, cached)
}

func TestInternalGatewayReplyOriginTokenReviewBackendFailureIsScoped(t *testing.T) {
	for _, failure := range []string{"create error", "status error", "authenticated status error"} {
		t.Run(failure, func(t *testing.T) {
			for _, request := range []struct {
				method, path string
				status       int
			}{
				{http.MethodGet, "/internal/v1/tasks/default/task/gateway-messages/origin", 503},
				{http.MethodGet, "/INTERNAL/v1/tasks/default/task/gateway-messages/origin/", 503},
				{http.MethodPost, "/internal/v1/tasks/default/task/gateway-messages/origin", 401},
				{http.MethodGet, "/internal/v1/tasks/default/task/gateway-messages/budget?requestID=id", 401},
				{http.MethodPost, "/internal/v1/tasks/default/task/gateway-messages", 401},
				{http.MethodPost, "/internal/v1/results/default/task", 401},
				{http.MethodGet, "/api/v1/tasks", 401},
			} {
				t.Run(request.method+request.path, func(t *testing.T) {
					f := newGatewayMessageAPIFixture(t)
					var reviews, reads atomic.Int32
					kube := interceptor.NewClient(f.h.k8sClient.(client.WithWatch), interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
							if review, ok := obj.(*authenticationv1.TokenReview); ok {
								reviews.Add(1)
								if failure == "create error" {
									return errors.New("private TokenReview backend diagnostic")
								}
								review.Status.Error = "private TokenReview status diagnostic"
								review.Status.Authenticated = failure == "authenticated status error"
								return nil
							}
							return c.Create(ctx, obj, opts...)
						},
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
							reads.Add(1)
							return c.Get(ctx, key, obj, opts...)
						},
					})
					server := NewServer(kube, nil, ServerConfig{GatewayService: f.service, ResultStore: f.db})
					_, cached := tokenCache.Load(getTokenHash(f.token))
					require.False(t, cached, "regression must exercise an uncached projected token")
					req := httptest.NewRequest(request.method, request.path, nil)
					req.Header.Set(AuthHeader, BearerPrefix+f.token)
					response, err := server.app.Test(req)
					require.NoError(t, err)
					defer func() { require.NoError(t, response.Body.Close()) }()
					require.Equal(t, request.status, response.StatusCode)
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NotContains(t, string(body), "private")
					require.Equal(t, int32(1), reviews.Load())
					if request.method == http.MethodGet && request.path == "/internal/v1/tasks/default/task/gateway-messages/origin" {
						httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							response, err := server.app.Test(r)
							if err != nil {
								t.Error(err)
								w.WriteHeader(500)
								return
							}
							defer func() { require.NoError(t, response.Body.Close()) }()
							w.WriteHeader(response.StatusCode)
							_, _ = io.Copy(w, response.Body)
						}))
						defer httpServer.Close()
						path := filepath.Join(t.TempDir(), "token")
						require.NoError(t, os.WriteFile(path, []byte(f.token), 0600))
						sender, err := workerclient.New(workerclient.Config{
							ControllerURL: httpServer.URL, Namespace: f.task.Namespace,
							TaskName: f.task.Name, TaskUID: string(f.task.UID), TokenFile: path,
						})
						require.NoError(t, err)
						require.ErrorIs(t, sender.AuthenticateOrigin(t.Context()), workerclient.ErrUnavailable)
						require.Equal(t, int32(6), reviews.Load(), "one direct request plus five bounded client attempts; no cached grant")
					}
					require.Zero(t, reads.Load(), "authentication failure must not reach caller authorization or handlers")
					_, cached = tokenCache.Load(getTokenHash(f.token))
					require.False(t, cached, "backend failure must not cache an identity")
				})
			}
		})
	}
}

func TestInternalGatewayReplyOriginWriterRevocation(t *testing.T) {
	f := newGatewayMessageAPIFixture(t)
	revoked := false
	fresh := interceptor.NewClient(f.h.k8sClient.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		err := c.Get(ctx, key, obj, opts...)
		if _, ok := obj.(*gatewayv1alpha1.Gateway); ok && !revoked {
			require.NoError(t, f.db.RevokeTaskJob(ctx, store.TaskJobIdentity{Namespace: f.task.Namespace, TaskUID: string(f.task.UID), JobUID: f.task.Status.JobUID}))
			revoked = true
		}
		return err
	}})
	f.h.apiReader = fresh
	f.service.APIReader = fresh
	originRequest(t, f, 403)
	require.True(t, revoked)
}
