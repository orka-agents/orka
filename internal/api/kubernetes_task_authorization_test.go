package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestHandlers_CreateTaskKubernetesRBACFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		kubeClient *kubefake.Clientset
	}{
		{name: "missing clientset"},
		{name: "subject access review error", kubeClient: denyingSubjectAccessReviewClient(t, errors.New("sar unavailable"), nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

			handlers := NewHandlers(HandlersConfig{Client: fakeClient, KubeClient: tt.kubeClient})
			app := fiber.New()
			app.Use(tokenReviewUserMiddleware(limitedTokenReviewUser("default")))
			app.Post("/tasks", handlers.CreateTask)

			resp := testJSONRequest(t, app, http.MethodPost, "/tasks", map[string]any{
				"name":  "fail-closed-task",
				"type":  "container",
				"image": "alpine:3.20",
			})
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			created := &corev1alpha1.Task{}
			err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "fail-closed-task", Namespace: "default"}, created)
			require.True(t, apierrors.IsNotFound(err), "failed authorization must not create a task")
		})
	}
}

func TestForkTaskRequiresKubernetesRBACForTokenReviewUser(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	appendTestTaskEvent(t, eventStore, "source-task", events.ExecutionEventTypeTaskStarted)
	source := testTask("default", "source-task")
	source.Spec.Type = corev1alpha1.TaskTypeAgent
	h, app := setupTaskEventHandlers(t, eventStore, source)
	h.clientset = denyingSubjectAccessReviewClient(t, nil, func(review *authorizationv1.SubjectAccessReview) {
		require.Equal(t, "system:serviceaccount:default:limited", review.Spec.User)
		require.NotNil(t, review.Spec.ResourceAttributes)
		require.Equal(t, "default", review.Spec.ResourceAttributes.Namespace)
		require.Equal(t, "create", review.Spec.ResourceAttributes.Verb)
		require.Equal(t, corev1alpha1.GroupVersion.Group, review.Spec.ResourceAttributes.Group)
		require.Equal(t, "tasks", review.Spec.ResourceAttributes.Resource)
	})
	app.Use(tokenReviewUserMiddleware(limitedTokenReviewUser("default")))
	app.Post("/api/v1/tasks/:id/fork", h.ForkTask)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/source-task/fork?namespace=default", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	tasks := &corev1alpha1.TaskList{}
	require.NoError(t, h.client.List(context.Background(), tasks, client.InNamespace("default")))
	require.Len(t, tasks.Items, 1, "denied fork should leave only the source task")
	require.Equal(t, "source-task", tasks.Items[0].Name)
}

func TestAuthorizeAndStampToolTaskCreateRequiresKubernetesRBACForTokenReviewUser(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "tool-task", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}

	err := authorizeAndStampToolTaskCreate(
		context.Background(),
		fakeClient,
		denyingSubjectAccessReviewClient(t, nil, nil),
		nil,
		ContextTokenAuthorizationConfig{},
		"chatToolCreateTask",
		limitedTokenReviewUser("default"),
		task,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not authorized to create tasks")
}

func TestCreateManualSecurityScanRequiresKubernetesRBACForTaskCreate(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan-1", Namespace: "demo"},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          securityTestRepoURL,
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithRuntimeObjects(scan).
		Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	securityStore := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{
		Client:        fakeClient,
		SecurityStore: securityStore,
		KubeClient:    denyingSubjectAccessReviewClient(t, nil, nil),
	})
	app := fiber.New()
	app.Use(tokenReviewUserMiddleware(limitedTokenReviewUser("demo")))
	app.Post("/security/repositories/:name/scans", handlers.CreateManualSecurityScan)

	req := httptest.NewRequest(http.MethodPost, "/security/repositories/scan-1/scans?namespace=demo", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	tasks := &corev1alpha1.TaskList{}
	require.NoError(t, fakeClient.List(context.Background(), tasks, client.InNamespace("demo")))
	require.Empty(t, tasks.Items, "denied security scan should not create a task")

	runs, _, err := securityStore.ListScanRuns(context.Background(), "demo", "scan-1", 10, "")
	require.NoError(t, err)
	require.Empty(t, runs, "denied security scan should not create a scan run")
}

func denyingSubjectAccessReviewClient(t *testing.T, reviewErr error, assertReview func(*authorizationv1.SubjectAccessReview)) *kubefake.Clientset {
	t.Helper()
	kubeClient := kubefake.NewSimpleClientset()
	kubeClient.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction := action.(k8stesting.CreateAction)
		review := createAction.GetObject().(*authorizationv1.SubjectAccessReview)
		if assertReview != nil {
			assertReview(review)
		}
		if reviewErr != nil {
			return true, nil, reviewErr
		}
		review.Status.Allowed = false
		return true, review, nil
	})
	return kubeClient
}

func limitedTokenReviewUser(namespace string) *UserInfo {
	return &UserInfo{
		Username:  "system:serviceaccount:" + namespace + ":limited",
		Groups:    []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace},
		Namespace: namespace,
		AuthType:  AuthTypeTokenReview,
	}
}

func tokenReviewUserMiddleware(userInfo *UserInfo) fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, userInfo)
		return c.Next()
	}
}
