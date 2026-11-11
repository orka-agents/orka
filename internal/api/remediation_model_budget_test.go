package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

type modelBudgetServiceStub struct {
	*remediationServiceStub
	claims []contract.ModelCallClaim
	err    error
}

func (s *modelBudgetServiceStub) ClaimModelCall(_ context.Context, claim contract.ModelCallClaim) error {
	s.claims = append(s.claims, claim)
	return s.err
}

func TestRemediationModelPermitRequiresExactNativeWorkerIdentity(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{name: "native", want: http.StatusNoContent},
		{name: "foreign pod", want: http.StatusForbidden},
		{name: "unbound identity", want: http.StatusForbidden},
		{name: "container task", want: http.StatusForbidden},
		{name: "unmanaged command", want: http.StatusForbidden},
		{name: "body authority", body: `{"maxCalls":100}`, want: http.StatusBadRequest},
		{name: "replayed permit", want: http.StatusConflict},
		{name: "no uncached reader", want: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := internalCallerAuthTask()
			task.Spec.Type = corev1alpha1.TaskTypeAI
			job := internalCallerAuthJob(task, "job-a", "job-uid")
			pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
			pod.Spec.Containers = []corev1.Container{{Name: "worker", Command: []string{"/worker"}, Args: []string{"--mode=ai"}}}
			user := internalCallerAuthWorkerUser(pod.Name, string(pod.UID))
			service := &modelBudgetServiceStub{remediationServiceStub: newRemediationServiceStub()}
			switch test.name {
			case "foreign pod":
				user = internalCallerAuthWorkerUser(pod.Name, "different-pod")
			case "unbound identity":
				user.AuthType = AuthTypeOIDC
			case "container task":
				task.Spec.Type = corev1alpha1.TaskTypeContainer
			case "unmanaged command":
				pod.Spec.Containers[0].Command = []string{"/other"}
			case "replayed permit":
				service.err = contract.ErrConflict
			}
			kube := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).WithObjects(task, job, pod).Build()
			h := NewHandlers(HandlersConfig{Client: kube, APIReader: kube, RemediationService: service})
			if test.name == "no uncached reader" {
				h.apiReader = nil
			}
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error { c.Locals(UserInfoContextKey, user); return c.Next() })
			app.Post("/internal/v1/tasks/:namespace/:taskName/remediation-model-call", h.ClaimRemediationModelCall)
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/task-a/remediation-model-call",
				strings.NewReader(test.body))
			response, err := app.Test(request)
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			require.Equal(t, test.want, response.StatusCode)
			require.Equal(t, "private, no-store", response.Header.Get("Cache-Control"))
			if test.want == http.StatusNoContent || test.name == "replayed permit" {
				require.Equal(t, []contract.ModelCallClaim{{
					Namespace: "default", TaskName: task.Name, TaskUID: string(task.UID),
					JobUID: string(job.UID), PodUID: string(pod.UID),
				}}, service.claims)
			} else {
				require.Empty(t, service.claims)
			}
		})
	}
}
