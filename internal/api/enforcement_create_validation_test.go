package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

type enforcementCreateOptionsClient struct {
	client.Client
	validation string
}

func (c *enforcementCreateOptionsClient) Create(
	ctx context.Context, object client.Object, options ...client.CreateOption,
) error {
	if _, ok := object.(*corev1alpha1.Task); ok {
		c.validation = (&client.CreateOptions{}).ApplyOptions(options).FieldValidation
	}
	return c.Client.Create(ctx, object, options...)
}

func TestEnforcedTaskCreateRequestsStrictFieldValidation(t *testing.T) {
	for _, protected := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "protected"}[protected], func(t *testing.T) {
			h, app := setupTestHandlers()
			capture := &enforcementCreateOptionsClient{Client: h.client}
			h.client = capture
			app.Post("/tasks", h.CreateTask)
			body := CreateTaskRequest{Name: "task", Namespace: "default", Type: corev1alpha1.TaskTypeAI,
				AI: &corev1alpha1.AISpec{Provider: "openai", Model: "test", Prompt: "hello"}}
			expected := ""
			if protected {
				body.Enforcement = testCompleteEnforcementRequirement()
				expected = metav1.FieldValidationStrict
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(raw))
			request.Header.Set("Content-Type", "application/json")
			response, err := app.Test(request)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusCreated, response.StatusCode)
			require.Equal(t, expected, capture.validation)
		})
	}
}

func TestEnforcedTaskOldSchemaRefusesBeforePersistence(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("run make test to supply the envtest control-plane binaries")
	}
	raw, err := os.ReadFile("../../config/crd/bases/core.orka.ai_tasks.yaml")
	require.NoError(t, err)
	var definition apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &definition))
	root := definition.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := root.Properties["spec"]
	delete(spec.Properties, "enforcement")
	spec.XValidations = nil
	root.Properties["spec"], root.XValidations = spec, nil
	environment := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{&definition}}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	kube, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	const namespace = "enforcement-validation"
	require.NoError(t, kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))

	control := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "pruning-control", Namespace: namespace},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI,
			AI:          &corev1alpha1.AISpec{Provider: "openai", Model: "test", Prompt: "hello"},
			Enforcement: testCompleteEnforcementRequirement()}}
	require.NoError(t, kube.Create(t.Context(), control, client.FieldValidation(metav1.FieldValidationIgnore)))
	persisted := &corev1alpha1.Task{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(control), persisted))
	require.Nil(t, persisted.Spec.Enforcement, "the old schema must reproduce actual Kubernetes pruning")
	require.Equal(t, control.Spec.AI, persisted.Spec.AI)

	h, app := setupTestHandlers()
	h.client = kube
	app.Post("/tasks", h.CreateTask)
	for _, protected := range []bool{false, true} {
		name := map[bool]string{false: "ordinary", true: "protected"}[protected]
		body := CreateTaskRequest{Name: name, Namespace: namespace, Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{Provider: "openai", Model: "test", Prompt: "hello"}}
		expected := http.StatusCreated
		if protected {
			body.Enforcement = testCompleteEnforcementRequirement()
			expected = http.StatusInternalServerError
		}
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, expected, response.StatusCode)
		err = kube.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &corev1alpha1.Task{})
		if protected {
			require.True(t, apierrors.IsNotFound(err), "protected Task must never be persisted: %v", err)
		} else {
			require.NoError(t, err)
		}
	}
}
