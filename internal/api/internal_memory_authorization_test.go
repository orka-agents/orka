package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func internalMemoryTaskToken(scopes ...string) *ContextToken {
	return &ContextToken{
		Scopes: scopes,
		TransactionContext: map[string]any{
			"namespace": "default",
			"taskName":  "memory-task",
			"taskUID":   "task-uid",
		},
	}
}

func testInternalMemoryRequest(
	t *testing.T,
	app *fiber.App,
	method, target, body string,
) *http.Response {
	t.Helper()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := app.Test(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestInternalMemoryCRUDRequiresTaskScopedTxnScopes(t *testing.T) {
	h, app, memoryStore, user := setupTestInternalMemoryHandlers(t)
	app.Get("/internal/v1/memories/:namespace", h.ListMemories)
	app.Post("/internal/v1/memories/:namespace", h.CreateMemory)
	app.Get("/internal/v1/memories/:namespace/:id", h.GetMemory)
	app.Put("/internal/v1/memories/:namespace/:id", h.UpdateMemory)
	app.Delete("/internal/v1/memories/:namespace/:id", h.DeleteMemory)
	app.Post("/internal/v1/memories/:namespace/:id/disable", h.DisableMemory)
	app.Post("/internal/v1/memories/:namespace/:id/enable", h.EnableMemory)

	for _, memory := range []*store.Memory{
		{ID: "mem-read", Namespace: "default", Content: "read content"},
		{ID: "mem-update", Namespace: "default", Content: "update content"},
		{ID: "mem-delete", Namespace: "default", Content: "delete content"},
		{ID: "mem-disable", Namespace: "default", Content: "disable content"},
		{ID: "mem-enable", Namespace: "default", Content: "enable content", Disabled: true},
	} {
		require.NoError(t, memoryStore.CreateMemory(context.Background(), memory))
	}

	tests := []struct {
		name        string
		method      string
		target      string
		body        string
		required    string
		wrong       string
		wantSuccess int
	}{
		{name: "list requires read", method: http.MethodGet, target: "/internal/v1/memories/default", required: ContextTokenScopeMemoryRead, wrong: ContextTokenScopeMemoryWrite, wantSuccess: http.StatusOK},
		{name: "get requires read", method: http.MethodGet, target: "/internal/v1/memories/default/mem-read", required: ContextTokenScopeMemoryRead, wrong: ContextTokenScopeMemoryWrite, wantSuccess: http.StatusOK},
		{name: "create requires write", method: http.MethodPost, target: "/internal/v1/memories/default", body: `{"id":"mem-created","content":"created content"}`, required: ContextTokenScopeMemoryWrite, wrong: ContextTokenScopeMemoryRead, wantSuccess: http.StatusCreated},
		{name: "update requires write", method: http.MethodPut, target: "/internal/v1/memories/default/mem-update", body: `{"content":"updated content"}`, required: ContextTokenScopeMemoryWrite, wrong: ContextTokenScopeMemoryRead, wantSuccess: http.StatusOK},
		{name: "delete requires write", method: http.MethodDelete, target: "/internal/v1/memories/default/mem-delete", required: ContextTokenScopeMemoryWrite, wrong: ContextTokenScopeMemoryRead, wantSuccess: http.StatusNoContent},
		{name: "disable requires write", method: http.MethodPost, target: "/internal/v1/memories/default/mem-disable/disable", required: ContextTokenScopeMemoryWrite, wrong: ContextTokenScopeMemoryRead},
		{name: "enable requires write", method: http.MethodPost, target: "/internal/v1/memories/default/mem-enable/enable", required: ContextTokenScopeMemoryWrite, wrong: ContextTokenScopeMemoryRead},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user.ContextToken = nil
			require.Equal(t, http.StatusForbidden,
				testInternalMemoryRequest(t, app, test.method, test.target, test.body).StatusCode)

			user.ContextToken = internalMemoryTaskToken(test.wrong)
			require.Equal(t, http.StatusForbidden,
				testInternalMemoryRequest(t, app, test.method, test.target, test.body).StatusCode)

			user.ContextToken = internalMemoryTaskToken(test.required)
			status := testInternalMemoryRequest(t, app, test.method, test.target, test.body).StatusCode
			if test.wantSuccess == 0 {
				require.NotEqual(t, http.StatusForbidden, status)
			} else {
				require.Equal(t, test.wantSuccess, status)
			}
		})
	}
}

func TestInternalGetMemoryDisabledInspectionRequiresOperateScope(t *testing.T) {
	h, app, memoryStore, user := setupTestInternalMemoryHandlers(t)
	app.Get("/internal/v1/memories/:namespace/:id", h.GetMemory)
	if err := memoryStore.CreateMemory(context.Background(), &store.Memory{
		ID: "mem-disabled", Namespace: "default", Content: "disabled content", Disabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryRead)
	response := testInternalMemoryRequest(t, app, http.MethodGet,
		"/internal/v1/memories/default/mem-disabled", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("default disabled GET status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var memory store.Memory
	if err := json.NewDecoder(response.Body).Decode(&memory); err != nil {
		t.Fatal(err)
	}
	if !memory.Disabled || memory.Content != "" {
		t.Fatalf("default disabled GET memory = %#v, want suppression metadata", memory)
	}

	response = testInternalMemoryRequest(t, app, http.MethodGet,
		"/internal/v1/memories/default/mem-disabled?includeDisabled=true", "")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only includeDisabled status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryRead, ContextTokenScopeMemoryOperate)
	response = testInternalMemoryRequest(t, app, http.MethodGet,
		"/internal/v1/memories/default/mem-disabled?includeDisabled=true", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("operator includeDisabled status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	memory = store.Memory{}
	if err := json.NewDecoder(response.Body).Decode(&memory); err != nil {
		t.Fatal(err)
	}
	if memory.Content != "disabled content" {
		t.Fatalf("operator includeDisabled memory = %#v, want content", memory)
	}
}

func TestInternalMemoryProposalActionsRequireTaskScopedTxnScopes(t *testing.T) {
	h, app, proposalStore, user := setupTestInternalMemoryHandlers(t)
	app.Post("/internal/v1/memory-proposals/:namespace", h.CreateMemoryProposal)
	app.Get("/internal/v1/memory-proposals/:namespace", h.ListMemoryProposals)
	app.Get("/internal/v1/memory-proposals/:namespace/:id", h.GetMemoryProposal)
	app.Post("/internal/v1/memory-proposals/:namespace/:id/review", h.ReviewMemoryProposal)
	app.Post("/internal/v1/memory-proposals/:namespace/:id/archive", h.ArchiveMemoryProposal)
	app.Post("/internal/v1/memory-proposals/:namespace/:id/apply", h.ApplyMemoryProposal)

	reviewProposal := &store.MemoryProposal{
		Namespace: "default", Type: "memory", Title: "Review proposal", Content: "reviewed content",
	}
	require.NoError(t, proposalStore.CreateMemoryProposal(context.Background(), reviewProposal))
	applyProposal := &store.MemoryProposal{
		Namespace: "default", Type: "memory", Title: "Apply proposal", Content: "applied content",
	}
	require.NoError(t, proposalStore.CreateMemoryProposal(context.Background(), applyProposal))
	require.NoError(t, proposalStore.ReviewMemoryProposal(context.Background(), store.MemoryProposalReview{
		Namespace: "default", ID: applyProposal.ID, Status: "accepted", Reviewer: "reviewer",
	}))
	archiveProposal := &store.MemoryProposal{
		Namespace: "default", Type: "memory", Title: "Archive proposal", Content: "archived content",
	}
	require.NoError(t, proposalStore.CreateMemoryProposal(context.Background(), archiveProposal))

	for _, target := range []string{
		"/internal/v1/memory-proposals/default",
		"/internal/v1/memory-proposals/default/" + reviewProposal.ID,
	} {
		user.ContextToken = nil
		require.Equal(t, http.StatusForbidden,
			testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)
		user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryWrite)
		require.Equal(t, http.StatusForbidden,
			testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)
		user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryRead)
		require.Equal(t, http.StatusOK,
			testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)
	}

	user.ContextToken = nil
	require.Equal(t, http.StatusForbidden, testInternalMemoryRequest(t, app, http.MethodPost,
		"/internal/v1/memory-proposals/default", `{"type":"memory","title":"Created proposal","content":"created content"}`).StatusCode)
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryOperate)
	require.Equal(t, http.StatusForbidden, testInternalMemoryRequest(t, app, http.MethodPost,
		"/internal/v1/memory-proposals/default", `{"type":"memory","title":"Created proposal","content":"created content"}`).StatusCode)
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryWrite)
	require.Equal(t, http.StatusCreated, testInternalMemoryRequest(t, app, http.MethodPost,
		"/internal/v1/memory-proposals/default", `{"type":"memory","title":"Created proposal","content":"created content"}`).StatusCode)

	reviewTarget := "/internal/v1/memory-proposals/default/" + reviewProposal.ID + "/review"
	user.ContextToken = nil
	require.Equal(t, http.StatusForbidden,
		testInternalMemoryRequest(t, app, http.MethodPost, reviewTarget, `{"status":"accepted"}`).StatusCode)
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryWrite)
	require.Equal(t, http.StatusForbidden,
		testInternalMemoryRequest(t, app, http.MethodPost, reviewTarget, `{"status":"accepted"}`).StatusCode)
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryOperate)
	require.Equal(t, http.StatusNoContent,
		testInternalMemoryRequest(t, app, http.MethodPost, reviewTarget, `{"status":"accepted"}`).StatusCode)

	archiveTarget := "/internal/v1/memory-proposals/default/" + archiveProposal.ID + "/archive"
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryRead)
	require.Equal(t, http.StatusForbidden,
		testInternalMemoryRequest(t, app, http.MethodPost, archiveTarget, `{}`).StatusCode)
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryWrite)
	require.Equal(t, http.StatusNoContent,
		testInternalMemoryRequest(t, app, http.MethodPost, archiveTarget, `{}`).StatusCode)

	applyTarget := "/internal/v1/memory-proposals/default/" + applyProposal.ID + "/apply"
	for _, scopes := range [][]string{
		nil,
		{ContextTokenScopeMemoryWrite},
		{ContextTokenScopeMemoryOperate},
	} {
		user.ContextToken = nil
		if scopes != nil {
			user.ContextToken = internalMemoryTaskToken(scopes...)
		}
		require.Equal(t, http.StatusForbidden,
			testInternalMemoryRequest(t, app, http.MethodPost, applyTarget, `{}`).StatusCode)
	}
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryWrite, ContextTokenScopeMemoryOperate)
	require.Equal(t, http.StatusOK,
		testInternalMemoryRequest(t, app, http.MethodPost, applyTarget, `{}`).StatusCode)
}

func TestInternalMemorySearchAndOperationReadsRequireTaskScopedTxnScopes(t *testing.T) {
	h, app, _, user := setupTestInternalMemoryHandlers(t)
	app.Post("/internal/v1/memories/:namespace/search", h.SearchMemories)
	app.Get("/internal/v1/memory-operations/:namespace", h.ListMemoryOperations)
	app.Get("/internal/v1/memory-operations/:namespace/:id", h.GetMemoryOperation)

	user.ContextToken = nil
	require.Equal(t, http.StatusForbidden, testInternalMemoryRequest(
		t, app, http.MethodPost, "/internal/v1/memories/default/search", `{"query":""}`,
	).StatusCode)
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryWrite)
	require.Equal(t, http.StatusForbidden, testInternalMemoryRequest(
		t, app, http.MethodPost, "/internal/v1/memories/default/search", `{"query":""}`,
	).StatusCode)
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryRead)
	require.Equal(t, http.StatusOK, testInternalMemoryRequest(
		t, app, http.MethodPost, "/internal/v1/memories/default/search", `{"query":""}`,
	).StatusCode)
	require.Equal(t, http.StatusForbidden, testInternalMemoryRequest(
		t, app, http.MethodPost, "/internal/v1/memories/default/search", `{"query":"","includeDisabled":true}`,
	).StatusCode)
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryRead, ContextTokenScopeMemoryOperate)
	require.Equal(t, http.StatusOK, testInternalMemoryRequest(
		t, app, http.MethodPost, "/internal/v1/memories/default/search", `{"query":"","includeDisabled":true}`,
	).StatusCode)

	for _, target := range []string{
		"/internal/v1/memory-operations/default",
		"/internal/v1/memory-operations/default/missing",
	} {
		user.ContextToken = nil
		require.Equal(t, http.StatusForbidden,
			testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)
		user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryWrite)
		require.Equal(t, http.StatusForbidden,
			testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)
		user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryRead)
		require.NotEqual(t, http.StatusForbidden,
			testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)
	}
}

func TestHarnessWrapperCrossNamespaceMemoryAccessRequiresBoundRuntimeRefTaskToken(t *testing.T) {
	const (
		controlNamespace = "orka-system"
		targetNamespace  = "team-runtime"
		runtimeTaskName  = "runtime-task"
		runtimeTaskUID   = "runtime-task-uid"
		runtimeTurnID    = "turn-1"
	)
	t.Setenv("POD_NAMESPACE", controlNamespace)
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")

	h, app, _, user := setupTestInternalMemoryHandlers(t)
	app.Get("/internal/v1/memories/:namespace", h.ListMemories)
	runtimeTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: targetNamespace, Name: runtimeTaskName, UID: types.UID(runtimeTaskUID),
			Annotations: map[string]string{
				harnessWrapperTurnIDAnnotation:  runtimeTurnID,
				harnessWrapperRuntimeAnnotation: "runtime-session-1",
				harnessWrapperStartedAnnotation: queryTrue,
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning,
			HarnessRuntime: &corev1alpha1.HarnessRuntimeStatus{
				RuntimeRefName: "remote-runtime",
			},
		},
	}
	require.NoError(t, h.k8sClient.Create(context.Background(), runtimeTask))
	nonRuntimeTask := runtimeTask.DeepCopy()
	nonRuntimeTask.Name = "job-backed-task"
	nonRuntimeTask.UID = types.UID("job-backed-task-uid")
	nonRuntimeTask.ResourceVersion = ""
	nonRuntimeTask.Status.HarnessRuntime = nil
	nonRuntimeTask.Status.JobName = "job-backed-task-job"
	require.NoError(t, h.k8sClient.Create(context.Background(), nonRuntimeTask))

	user.AuthType = AuthTypeTokenReview
	user.Username = "system:serviceaccount:" + controlNamespace + ":agent-harness-wrapper"
	user.Namespace = controlNamespace
	user.Extra = nil
	target := "/internal/v1/memories/" + targetNamespace
	taskToken := func(namespace, name, uid string) *ContextToken {
		return &ContextToken{
			Scopes: []string{ContextTokenScopeMemoryRead},
			TransactionContext: map[string]any{
				"namespace": namespace,
				"taskName":  name,
				"taskUID":   uid,
			},
		}
	}

	user.ContextToken = nil
	require.Equal(t, http.StatusForbidden,
		testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)

	wrongScopeToken := taskToken(targetNamespace, runtimeTaskName, runtimeTaskUID)
	wrongScopeToken.Scopes = []string{ContextTokenScopeMemoryWrite}
	for name, token := range map[string]*ContextToken{
		"wrong scope":     wrongScopeToken,
		"wrong namespace": taskToken("other-team", runtimeTaskName, runtimeTaskUID),
		"wrong task name": taskToken(targetNamespace, "other-task", runtimeTaskUID),
		"wrong task uid":  taskToken(targetNamespace, runtimeTaskName, "other-uid"),
		"non-runtime task": taskToken(
			targetNamespace, nonRuntimeTask.Name, string(nonRuntimeTask.UID),
		),
	} {
		t.Run(name, func(t *testing.T) {
			user.ContextToken = token
			require.Equal(t, http.StatusForbidden,
				testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)
		})
	}

	user.ContextToken = taskToken(targetNamespace, runtimeTaskName, runtimeTaskUID)
	require.Equal(t, http.StatusOK,
		testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)

	user.Username = "system:serviceaccount:" + controlNamespace + ":other-service-account"
	require.Equal(t, http.StatusForbidden,
		testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)

	user.Username = "system:serviceaccount:" + targetNamespace + ":agent-harness-wrapper"
	user.Namespace = targetNamespace
	require.Equal(t, http.StatusForbidden,
		testInternalMemoryRequest(t, app, http.MethodGet, target, "").StatusCode)
}

func TestInternalMemoryAuthorizationOffPreservesVerifiedWorkloadCompatibility(t *testing.T) {
	h, app, _, user := setupTestInternalMemoryHandlers(t)
	h.contextTokenAuthorization = ContextTokenAuthorizationConfig{Mode: ContextTokenAuthorizationModeOff}
	app.Get("/internal/v1/memories/:namespace", h.ListMemories)
	user.ContextToken = nil

	require.Equal(t, http.StatusOK,
		testInternalMemoryRequest(t, app, http.MethodGet, "/internal/v1/memories/default", "").StatusCode)
}

func TestInternalMemoryAuthorizationRejectsRecreatedTaskUID(t *testing.T) {
	h, app, _, user := setupTestInternalMemoryHandlers(t)
	app.Get("/internal/v1/memories/:namespace", h.ListMemories)
	user.ContextToken = internalMemoryTaskToken(ContextTokenScopeMemoryRead)
	user.ContextToken.TransactionContext["taskUID"] = "recreated-task-uid"

	require.Equal(t, http.StatusForbidden,
		testInternalMemoryRequest(t, app, http.MethodGet, "/internal/v1/memories/default", "").StatusCode)
}
