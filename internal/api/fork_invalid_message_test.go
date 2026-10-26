package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestInvalidObjectMessageNamesRejectedFields(t *testing.T) {
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "core.orka.ai", Kind: "Task"}, "Bad_Name", field.ErrorList{
		field.Invalid(field.NewPath("metadata", "name"), "Bad_Name", "must be a lowercase RFC 1123 subdomain"),
		field.Required(field.NewPath("spec", "prompt"), ""),
	})
	require.Equal(t,
		`forked task is invalid: metadata.name: Invalid value: "Bad_Name": must be a lowercase RFC 1123 subdomain; spec.prompt: Required value`,
		invalidObjectMessage("forked task is invalid", invalid))
	require.Equal(t, "forked task is invalid", invalidObjectMessage("forked task is invalid", errors.New("not a status")))
}

func TestInvalidObjectErrorMapsAdmissionDenialsOnly(t *testing.T) {
	gr := schema.GroupResource{Group: "core.orka.ai", Resource: "agents"}
	denied := apierrors.NewForbidden(gr, "coder", errors.New(`admission webhook "orka-admission.orka.ai" denied the request: harness-v2 built-in Agents require spec.model.name`))
	mapped := invalidObjectError("agent", denied)
	require.Error(t, mapped)
	var fiberErr *fiber.Error
	require.ErrorAs(t, mapped, &fiberErr)
	require.Equal(t, fiber.StatusBadRequest, fiberErr.Code)
	require.Contains(t, fiberErr.Message, "require spec.model.name")

	// The API server's own RBAC denial is not the caller's mistake.
	rbac := apierrors.NewForbidden(gr, "coder", errors.New(`User "system:serviceaccount:orka-system:orka-controller-manager" cannot create resource "agents"`))
	require.NoError(t, invalidObjectError("agent", rbac))
	require.NoError(t, invalidObjectError("agent", errors.New("boom")))
}

func TestKubernetesInvalidWritesReturnBadRequest(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	demo := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "demo"} }
	reject := func(obj client.Object) error {
		return apierrors.NewInvalid(schema.GroupKind{Group: corev1alpha1.GroupVersion.Group, Kind: "Object"}, obj.GetName(), field.ErrorList{
			field.Invalid(field.NewPath("spec", "limit"), -1, "should be greater than or equal to 0"),
		})
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1alpha1.Agent{ObjectMeta: demo("agent")},
			&corev1alpha1.Agent{ObjectMeta: demo("reviewer"), Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude}}},
			&corev1alpha1.Skill{ObjectMeta: demo("skill")},
			&corev1alpha1.Provider{ObjectMeta: demo("provider")},
			&corev1alpha1.Tool{ObjectMeta: demo("tool")},
			securityAuthzTestRepositoryScan("scan", securityTestRepoURL),
			&corev1alpha1.RepositoryMonitor{ObjectMeta: demo("monitor"), Spec: corev1alpha1.RepositoryMonitorSpec{RepoURL: monitorTestRepoURL}},
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return reject(obj)
			},
			Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
				return reject(obj)
			},
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				return reject(obj)
			},
		}).
		Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{Client: fakeClient, SecurityStore: store, RepositoryMonitorStore: store})

	app := fiber.New()
	app.Post("/tasks", handlers.CreateTask)
	app.Post("/agents", handlers.CreateAgent)
	app.Put("/agents/:name", handlers.UpdateAgent)
	app.Post("/skills", handlers.CreateSkill)
	app.Put("/skills/:name", handlers.UpdateSkill)
	app.Post("/providers", handlers.CreateProvider)
	app.Put("/providers/:name", handlers.UpdateProvider)
	app.Post("/tools", handlers.CreateTool)
	app.Put("/tools/:name", handlers.UpdateTool)
	app.Post("/security/repositories", handlers.CreateRepositoryScan)
	app.Put("/security/repositories/:name", handlers.UpdateRepositoryScan)
	app.Post("/monitors/repositories", handlers.CreateRepositoryMonitor)
	app.Put("/monitors/repositories/:name", handlers.UpdateRepositoryMonitor)

	scanSpec := `{"repoURL":"` + securityTestRepoURL + `","analysisAgentRef":{"name":"analysis"}}`
	monitorSpec := `{"repoURL":"` + monitorTestRepoURL + `","agents":{"reviewer":{"name":"reviewer"}}}`
	for _, tt := range []struct {
		method, path, body, kind string
	}{
		{http.MethodPost, "/tasks", `{"name":"bad","namespace":"demo","type":"container","image":"alpine:3.20"}`, "task"},
		{http.MethodPost, "/agents", `{"name":"bad","namespace":"demo","spec":{}}`, "agent"},
		{http.MethodPut, "/agents/agent?namespace=demo", `{"spec":{}}`, "agent"},
		{http.MethodPost, "/skills", `{"name":"bad","namespace":"demo","spec":{}}`, "skill"},
		{http.MethodPut, "/skills/skill?namespace=demo", `{"spec":{}}`, "skill"},
		{http.MethodPost, "/providers", `{"name":"bad","namespace":"demo","spec":{}}`, "provider"},
		{http.MethodPut, "/providers/provider?namespace=demo", `{"spec":{}}`, "provider"},
		{http.MethodPost, "/tools", `{"name":"bad","namespace":"demo","spec":{}}`, "tool"},
		{http.MethodPut, "/tools/tool?namespace=demo", `{"spec":{}}`, "tool"},
		{http.MethodPost, "/security/repositories", `{"name":"Bad_Name","namespace":"demo","spec":` + scanSpec + `}`, "repository scan"},
		{http.MethodPut, "/security/repositories/scan?namespace=demo", `{"spec":` + scanSpec + `}`, "repository scan"},
		{http.MethodPost, "/monitors/repositories", `{"name":"bad","namespace":"demo","spec":` + monitorSpec + `}`, "repository monitor"},
		{http.MethodPut, "/monitors/repositories/monitor?namespace=demo", `{"spec":` + monitorSpec + `}`, "repository monitor"},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			defer resp.Body.Close() //nolint:errcheck
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
			require.Equal(t, tt.kind+` is invalid: spec.limit: Invalid value: -1: should be greater than or equal to 0`, string(body))
		})
	}
}
