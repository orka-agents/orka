/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	testEnvToken       = "env-token"
	testMyOrgOwner     = "myorg"
	testMyOrgRepo      = "myrepo"
	testMyOrgRepoScope = "myorg/myrepo"
)

type countingClient struct {
	client.Client
	taskGets int
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1alpha1.Task); ok {
		c.taskGets++
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestResolveReadRepoAndToken_DirectRepoURL_HTTPS(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	owner, repo, token, baseURL, err := resolveReadRepoAndToken(context.Background(), nil, "list_pull_requests", "", testMyOrgRepoURL, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != testMyOrgOwner || repo != testMyOrgRepo {
		t.Errorf("got owner=%q repo=%q, want %s", owner, repo, testMyOrgRepoScope)
	}
	if token != testEnvToken {
		t.Errorf("got token=%q, want env-token", token)
	}
	if baseURL != githubAPIBaseURL {
		t.Errorf("got baseURL=%q, want %q", baseURL, githubAPIBaseURL)
	}
}

func TestResolveReadRepoAndToken_DirectRepoURL_SSH(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	owner, repo, token, _, err := resolveReadRepoAndToken(context.Background(), nil, "list_pull_requests", "", "git@github.com:myorg/myrepo.git", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != testMyOrgOwner || repo != testMyOrgRepo {
		t.Errorf("got owner=%q repo=%q, want %s", owner, repo, testMyOrgRepoScope)
	}
	if token != testEnvToken {
		t.Errorf("got token=%q, want env-token", token)
	}
}

func TestResolveReadRepoAndToken_EnvVarFallback(t *testing.T) {
	t.Setenv("ORKA_GIT_REPO", "https://github.com/envorg/envrepo")
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	owner, repo, token, _, err := resolveReadRepoAndToken(context.Background(), nil, "list_pull_requests", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "envorg" || repo != "envrepo" {
		t.Errorf("got owner=%q repo=%q, want envorg/envrepo", owner, repo)
	}
	if token != testEnvToken {
		t.Errorf("got token=%q, want env-token", token)
	}
}

func TestResolveReadRepoAndToken_TaskName(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testMyTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:           "https://github.com/taskorg/taskrepo",
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: testGitCredsSecretName},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte("task-secret-token")},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	owner, repo, token, baseURL, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", testMyTaskName, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "taskorg" || repo != "taskrepo" {
		t.Errorf("got owner=%q repo=%q, want taskorg/taskrepo", owner, repo)
	}
	if token != "task-secret-token" {
		t.Errorf("got token=%q, want task-secret-token", token)
	}
	if baseURL != githubAPIBaseURL {
		t.Errorf("got baseURL=%q, want %q", baseURL, githubAPIBaseURL)
	}
}

func TestResolveGitHubCredentials_RoleSeparation(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(workerenv.GitHubToken, "environment-token-must-not-be-used")

	const forgeKey = "forge-api-token"
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "role-separated-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:                      "https://github.com/source/repository",
				PublicationGitRepo:           "https://github.com/target/repository",
				ReadCredentialRef:            &corev1alpha1.WorkspaceCredentialReference{Name: "source-read"},
				PublicationReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "target-read"},
				PublicationCredentialRef:     &corev1alpha1.WorkspaceCredentialReference{Name: "target-write"},
				ForgeCredentialRef: &corev1alpha1.WorkspaceCredentialReference{
					Name: "forge-api",
					Key:  forgeKey,
				},
			},
		},
	}
	objects := []client.Object{
		task,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "source-read", Namespace: defaultNamespace}, Data: map[string][]byte{tokenKey: []byte("source-read-token")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "target-read", Namespace: defaultNamespace}, Data: map[string][]byte{tokenKey: []byte("target-read-token")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "target-write", Namespace: defaultNamespace}, Data: map[string][]byte{tokenKey: []byte("target-write-token")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "forge-api", Namespace: defaultNamespace}, Data: map[string][]byte{
			tokenKey: []byte("wrong-default-sibling"),
			forgeKey: []byte("forge-token"),
		}},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	tests := []struct {
		name    string
		resolve func() (string, error)
		want    string
	}{
		{
			name: "source read",
			resolve: func() (string, error) {
				_, _, token, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", task.Name, "", "")
				return token, err
			},
			want: "source-read-token",
		},
		{
			name: "publication read",
			resolve: func() (string, error) {
				_, _, token, _, err := resolveScopedReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", task.Name, task.Spec.Workspace.PublicationGitRepo, "")
				return token, err
			},
			want: "target-read-token",
		},
		{
			name: "source forge mutation",
			resolve: func() (string, error) {
				_, _, token, _, err := resolveForgeRepoAndToken(context.Background(), k8sClient, "list_pull_requests", task.Name, "", "")
				return token, err
			},
			want: "forge-token",
		},
		{
			name: "publication forge mutation",
			resolve: func() (string, error) {
				_, _, token, _, err := resolveScopedForgeRepoAndToken(context.Background(), k8sClient, "list_pull_requests", task.Name, task.Spec.Workspace.PublicationGitRepo, "")
				return token, err
			},
			want: "forge-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := tt.resolve()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if token != tt.want {
				t.Error("resolved token from the wrong credential role")
			}
		})
	}
}

func TestResolveForgeRepoAndToken_TaskWithoutForgeCredentialFailsClosed(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(workerenv.GitHubToken, "environment-token-must-not-be-used")

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "no-forge-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:                  testMyOrgRepoURL,
				ReadCredentialRef:        &corev1alpha1.WorkspaceCredentialReference{Name: "source-read"},
				PublicationCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "target-write"},
			},
		},
	}
	objects := []client.Object{
		task,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "source-read", Namespace: defaultNamespace}, Data: map[string][]byte{tokenKey: []byte("source-read-token")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "target-write", Namespace: defaultNamespace}, Data: map[string][]byte{tokenKey: []byte("target-write-token")}},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	_, _, _, _, err := resolveForgeRepoAndToken(context.Background(), k8sClient, "list_pull_requests", task.Name, "", "")
	if err == nil {
		t.Fatal("expected missing forgeCredentialRef error")
	}
	if !contains(err.Error(), "workspace has no forgeCredentialRef configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveReadRepoAndToken_TaskNameAndRepoURLLoadsTaskOnce(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testMyTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:           testMyOrgRepoURL,
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: testGitCredsSecretName},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte("task-secret-token")},
	}

	k8sClient := &countingClient{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build(),
	}

	owner, repo, token, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", testMyTaskName, testMyOrgRepoURL, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != testMyOrgOwner || repo != testMyOrgRepo {
		t.Errorf("got owner=%q repo=%q, want %s", owner, repo, testMyOrgRepoScope)
	}
	if token != "task-secret-token" {
		t.Errorf("got token=%q, want task-secret-token", token)
	}
	if k8sClient.taskGets != 1 {
		t.Errorf("task Get calls = %d, want 1", k8sClient.taskGets)
	}
}

func TestResolveReadRepoAndToken_TaskName_MissingCustomKeyDoesNotUseSiblings(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(workerenv.GitHubToken, testEnvToken)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "pw-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:           "https://github.com/pworg/pwrepo",
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "git-pw", Key: "github-api-token"},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-pw", Namespace: defaultNamespace},
		Data: map[string][]byte{
			tokenKey:    []byte("default-sibling-token"),
			passwordKey: []byte("password-sibling-token"),
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	_, _, _, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", "pw-task", "", "")
	if err == nil {
		t.Fatal("expected configured-key error")
	}
	if !contains(err.Error(), `does not contain configured key "github-api-token"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveReadRepoAndToken_TaskName_CustomKey(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	const customKey = "github-api-token"
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-key-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo: "https://github.com/custom/custom",
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{
					Name: "custom-key-secret",
					Key:  customKey,
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-key-secret", Namespace: defaultNamespace},
		Data: map[string][]byte{
			tokenKey:  []byte("wrong-sibling-token"),
			customKey: []byte("custom-key-token"),
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	_, _, token, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", "custom-key-task", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "custom-key-token" {
		t.Error("resolved token from the wrong Secret key")
	}
}

func TestResolveReadRepoAndToken_TaskName_EmptySelectedKeyDoesNotUseSibling(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	const customKey = "github-api-token"
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-key-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo: "https://github.com/empty/empty",
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{
					Name: "empty-key-secret",
					Key:  customKey,
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-key-secret", Namespace: defaultNamespace},
		Data: map[string][]byte{
			tokenKey:  []byte("wrong-sibling-token"),
			customKey: []byte(" \n"),
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	_, _, _, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", "empty-key-task", "", "")
	if err == nil {
		t.Fatal("expected empty configured-key error")
	}
	if !contains(err.Error(), `contains an empty configured key "github-api-token"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestResolveReadRepoAndToken_TaskName_ToolContextNamespace pins the namespace
// resolution order for the proxy use case: when the controller process runs
// create_pull_request server-side, it has no per-request env vars, so the
// helper must read namespace from ToolContext (set by the proxy from the
// request context). Falling back to ORKA_TASK_NAMESPACE / "default" silently
// resolved the wrong Task and broke the chat-to-PR demo after every retry.
func TestResolveReadRepoAndToken_TaskName_ToolContextNamespace(t *testing.T) {
	// Intentionally point env var at the wrong namespace to prove ToolContext wins.
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv("GITHUB_TOKEN", "")

	const proxyNamespace = "demo-magic"

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-task", Namespace: proxyNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:           "https://github.com/proxorg/proxyrepo",
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "proxy-creds"},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-creds", Namespace: proxyNamespace},
		Data:       map[string][]byte{tokenKey: []byte("proxy-token")},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	ctx := WithToolContext(context.Background(), &ToolContext{Namespace: proxyNamespace})

	owner, repo, token, _, err := resolveReadRepoAndToken(ctx, k8sClient, "list_pull_requests", "proxy-task", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "proxorg" || repo != "proxyrepo" {
		t.Errorf("got owner=%q repo=%q, want proxorg/proxyrepo", owner, repo)
	}
	if token != "proxy-token" {
		t.Errorf("got token=%q, want proxy-token", token)
	}
}

func TestResolveReadRepoAndToken_TokenFromFile(t *testing.T) {
	// Create a temp directory to simulate /secrets/git/token
	tmpDir := t.TempDir()
	tokenFile := filepath.Join(tmpDir, tokenKey)
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("failed to write token file: %v", err)
	}

	// We can't easily override /secrets/git/token path, so test resolveToken directly
	// by setting GITHUB_TOKEN and verifying it's returned as fallback
	t.Setenv("GITHUB_TOKEN", "env-fallback-token")

	token := resolveToken()
	if token != "env-fallback-token" {
		t.Errorf("got token=%q, want env-fallback-token", token)
	}
}

func TestResolveReadRepoAndToken_TokenFromEnvVar(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "my-gh-token")

	owner, repo, token, _, err := resolveReadRepoAndToken(context.Background(), nil, "list_pull_requests", "", testOrgTestRepoURL, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "testorg" || repo != "testrepo" {
		t.Errorf("got owner=%q repo=%q, want testorg/testrepo", owner, repo)
	}
	if token != "my-gh-token" {
		t.Errorf("got token=%q, want my-gh-token", token)
	}
}

func TestResolveReadRepoAndToken_BaseURLOverride(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")

	_, _, _, baseURL, err := resolveReadRepoAndToken(context.Background(), nil, "list_pull_requests", "", "https://github.com/o/r", "http://localhost:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if baseURL != "http://localhost:8080" {
		t.Errorf("got baseURL=%q, want http://localhost:8080", baseURL)
	}
}

func TestResolveReadRepoAndToken_ErrorNoRepoURL(t *testing.T) {
	// Ensure ORKA_GIT_REPO is not set
	t.Setenv("ORKA_GIT_REPO", "")

	_, _, _, _, err := resolveReadRepoAndToken(context.Background(), nil, "list_pull_requests", "", "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if expected := "no repo_url, task_name, or ORKA_GIT_REPO provided"; err.Error() != expected {
		t.Errorf("got error %q, want %q", err.Error(), expected)
	}
}

func TestResolveReadRepoAndToken_ErrorInvalidURL(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")

	_, _, _, _, err := resolveReadRepoAndToken(context.Background(), nil, "list_pull_requests", "", "not-a-valid-url", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "unsupported GitHub URL format") {
		t.Errorf("got error %q, want it to contain 'unsupported GitHub URL format'", err.Error())
	}
}

func TestResolveReadRepoAndToken_ErrorTaskNotFound(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, _, _, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", "nonexistent-task", "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "failed to get task nonexistent-task") {
		t.Errorf("got error %q, want it to contain 'failed to get task nonexistent-task'", err.Error())
	}
}

func TestResolveReadRepoAndToken_ErrorNoToken(t *testing.T) {
	// Clear all token sources
	t.Setenv("GITHUB_TOKEN", "")

	_, _, _, _, err := resolveReadRepoAndToken(context.Background(), nil, "list_pull_requests", "", "https://github.com/o/r", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "could not resolve GitHub token") {
		t.Errorf("got error %q, want it to contain 'could not resolve GitHub token'", err.Error())
	}
}

func TestResolveReadRepoAndToken_ErrorTaskNoWorkspace(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testNoWorkspaceTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()

	_, _, _, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", testNoWorkspaceTaskName, "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "does not have workspace configuration") {
		t.Errorf("got error %q, want it to contain 'does not have workspace configuration'", err.Error())
	}
}

func TestResolveReadRepoAndToken_TaskWithoutGitSecretRefFallsBackToEnvToken(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "no-secret-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo: testOrgRepoURL,
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()

	owner, repo, token, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", "no-secret-task", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "org" || repo != "repo" {
		t.Errorf("got owner=%q repo=%q, want org/repo", owner, repo)
	}
	if token != testEnvToken {
		t.Errorf("got token=%q, want %q", token, testEnvToken)
	}
}

func TestResolveReadRepoAndToken_RepoURLMatchingTaskNameUsesTaskToken(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "some-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:           "https://github.com/task-org/task-repo",
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: testGitCredsSecretName},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte("task-token")},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	owner, repo, token, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", "some-task", "https://github.com/task-org/task-repo", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "task-org" || repo != "task-repo" {
		t.Errorf("got owner=%q repo=%q, want task-org/task-repo", owner, repo)
	}
	if token != "task-token" {
		t.Errorf("got token=%q, want task-token", token)
	}
}

func TestResolveReadRepoAndToken_RepoURLMismatchRejectsTaskToken(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "some-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:           "https://github.com/task-org/task-repo",
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: testGitCredsSecretName},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte("task-token")},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	_, _, _, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", "some-task", "https://github.com/url-org/url-repo", "")
	if err == nil {
		t.Fatal("expected repo scope mismatch error")
	}
	if !contains(err.Error(), "does not match permitted repository scope") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveReadRepoAndToken_RepoURLWithMissingTaskFailsClosed(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, _, _, _, err := resolveReadRepoAndToken(context.Background(), k8sClient, "list_pull_requests", "nonexistent-task", "https://github.com/url-org/url-repo", "")
	if err == nil {
		t.Fatal("expected task lookup error")
	}
	if !contains(err.Error(), "failed to get task nonexistent-task") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type fakeLinkedAccounts struct {
	tool       string
	credential LinkedAccountCredential
	bound      bool
	err        error
	calls      int
}

func (f *fakeLinkedAccounts) BuiltinToolCredential(_ context.Context, toolName string) (LinkedAccountCredential, bool, error) {
	f.calls++
	f.tool = toolName
	return f.credential, f.bound, f.err
}

// linkedAccountFixture is a Task with a workspace and read Secret, in a
// fake client, for the linked-account helper tests.
func linkedAccountFixture(t *testing.T) (client.Client, *corev1alpha1.Task) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testMyTaskName, Namespace: defaultNamespace, UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Workspace: &corev1alpha1.WorkspaceConfig{
			GitRepo: "https://github.com/taskorg/taskrepo", ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: testGitCredsSecretName},
		}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace}, Data: map[string][]byte{tokenKey: []byte("task-secret-token")}}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build(), task
}

func TestResolveRepoAndToken_LinkedAccountFirst(t *testing.T) {
	k8sClient, task := linkedAccountFixture(t)

	// A bound link supplies the token; the Task's Secret is never read and
	// the workspace still scopes the repository.
	linked := &fakeLinkedAccounts{credential: LinkedAccountCredential{AccessToken: "linked-token", Provider: "github"}, bound: true}
	ctx := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: testMyTaskName, LinkedAccounts: linked, RequireGitHubTaskCredentials: true})
	owner, repo, token, _, err := resolveScopedReadRepoAndToken(ctx, k8sClient, "list_pull_requests", "", "", "")
	if err != nil || owner != "taskorg" || repo != "taskrepo" || token != "linked-token" || linked.tool != "list_pull_requests" {
		t.Fatalf("linked: %s/%s token=%q err=%v tool=%q", owner, repo, token, err, linked.tool)
	}
	if _, _, _, _, err := resolveScopedReadRepoAndToken(ctx, k8sClient, "list_pull_requests", "", "https://github.com/other/repo", ""); err == nil {
		t.Fatal("a linked account must not widen the repository scope")
	}
	// Nor may another Task's workspace lend its scope to the linked token.
	other := task.DeepCopy()
	other.Name, other.ResourceVersion = "other-task", ""
	other.Spec.Workspace.GitRepo = "https://github.com/other/repo"
	if err := k8sClient.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := resolveScopedReadRepoAndToken(ctx, k8sClient, "list_pull_requests", "other-task", "", ""); err == nil || !strings.Contains(err.Error(), "must name the current task") {
		t.Fatalf("foreign task_name err = %v", err)
	}
	if _, repo, _, _, err := resolveScopedReadRepoAndToken(ctx, k8sClient, "list_pull_requests", testMyTaskName, "", ""); err != nil || repo != "taskrepo" {
		t.Fatalf("own task_name: repo=%q err=%v", repo, err)
	}
	// Outside a Task (chat and the proxies) only a Task this turn created
	// for the same person may lend its repository scope to the link.
	requester := &corev1alpha1.RequestedBy{Issuer: "https://issuer.example.test", Subject: "alice"}
	outsideCtx := &ToolContext{Namespace: defaultNamespace, LinkedAccounts: linked, Requester: requester, CreatedTasks: NewCreatedTasks()}
	outside := WithToolContext(context.Background(), outsideCtx)
	if _, _, _, _, err := resolveScopedReadRepoAndToken(outside, k8sClient, "list_pull_requests", testMyTaskName, "", ""); err == nil || !strings.Contains(err.Error(), "created in this conversation") {
		t.Fatalf("task_name outside a task err = %v", err)
	}
	// Naming no Task outside one is refused too: the readers that take an
	// unscoped repo_url would otherwise point the link at any repository.
	if _, _, _, _, err := resolveReadRepoAndToken(outside, k8sClient, "get_issue", "", "https://github.com/other/repo", ""); err == nil || !strings.Contains(err.Error(), "created in this conversation") {
		t.Fatalf("unscoped repo_url outside a task err = %v", err)
	}
	// A Task this turn created in another namespace is resolved from the
	// record (name alone is what the tool receives) and read from there.
	elsewhere := task.DeepCopy()
	elsewhere.Name, elsewhere.Namespace, elsewhere.UID, elsewhere.ResourceVersion = "elsewhere-task", "elsewhere", "other-uid", ""
	elsewhere.Spec.RequestedBy = requester
	elsewhere.Spec.Workspace.GitRepo = "https://github.com/elseorg/elserepo"
	if err := k8sClient.Create(context.Background(), elsewhere); err != nil {
		t.Fatal(err)
	}
	outsideCtx.RecordCreatedTask(elsewhere)
	if _, repo, token, _, err := resolveScopedReadRepoAndToken(outside, k8sClient, "list_pull_requests", "elsewhere-task", "", ""); err != nil || repo != "elserepo" || token != "linked-token" {
		t.Fatalf("other-namespace created task: repo=%q token=%q err=%v", repo, token, err)
	}
	// A workspace edited after creation no longer scopes the linked token.
	drifted := &corev1alpha1.Task{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "elsewhere-task", Namespace: "elsewhere"}, drifted); err != nil {
		t.Fatal(err)
	}
	drifted.Spec.Workspace.GitRepo = "https://github.com/victim/repo"
	if err := k8sClient.Update(context.Background(), drifted); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := resolveScopedReadRepoAndToken(outside, k8sClient, "list_pull_requests", "elsewhere-task", "", ""); err == nil || !strings.Contains(err.Error(), "workspace changed") {
		t.Fatalf("drifted workspace err = %v", err)
	}
	drifted.Spec.Workspace.GitRepo = "https://github.com/elseorg/elserepo"
	if err := k8sClient.Update(context.Background(), drifted); err != nil {
		t.Fatal(err)
	}
	// The same name recorded in two namespaces is ambiguous and refused.
	twin := elsewhere.DeepCopy()
	twin.Namespace, twin.UID = "third", "third-uid"
	outsideCtx.RecordCreatedTask(twin)
	if _, _, _, _, err := resolveScopedReadRepoAndToken(outside, k8sClient, "list_pull_requests", "elsewhere-task", "", ""); err == nil || !strings.Contains(err.Error(), "created in this conversation") {
		t.Fatalf("ambiguous created task err = %v", err)
	}
	// A recorded identity that no longer matches the live object is refused.
	replaced := task.DeepCopy()
	replaced.UID = "replaced-uid"
	outsideCtx.RecordCreatedTask(replaced)
	if _, _, _, _, err := resolveScopedReadRepoAndToken(outside, k8sClient, "list_pull_requests", testMyTaskName, "", ""); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("replaced task err = %v", err)
	}
	// A Task created this turn but stamped for somebody else (or nobody) is refused.
	outsideCtx.RecordCreatedTask(task)
	if _, _, _, _, err := resolveScopedReadRepoAndToken(outside, k8sClient, "list_pull_requests", testMyTaskName, "", ""); err == nil || !strings.Contains(err.Error(), "not requested by the person") {
		t.Fatalf("unstamped created task err = %v", err)
	}
	stamped := task.DeepCopy()
	stamped.Spec.RequestedBy = requester
	if err := k8sClient.Update(context.Background(), stamped); err != nil {
		t.Fatal(err)
	}
	if _, repo, token, _, err := resolveScopedReadRepoAndToken(outside, k8sClient, "list_pull_requests", testMyTaskName, "", ""); err != nil || repo != "taskrepo" || token != "linked-token" {
		t.Fatalf("created task scope: repo=%q token=%q err=%v", repo, token, err)
	}
	// No record set at all (a context the API did not build) never accepts one.
	bare := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, LinkedAccounts: linked, Requester: requester})
	if _, _, _, _, err := resolveScopedReadRepoAndToken(bare, k8sClient, "list_pull_requests", testMyTaskName, "", ""); err == nil || !strings.Contains(err.Error(), "created in this conversation") {
		t.Fatalf("bare context err = %v", err)
	}
}

func TestResolveRepoAndToken_LinkedAccountChildAndTransactionScope(t *testing.T) {
	k8sClient, task := linkedAccountFixture(t)
	linked := &fakeLinkedAccounts{credential: LinkedAccountCredential{AccessToken: "linked-token", Provider: "github"}, bound: true}
	// A child this Task controls may supply the scope (a coordinator opening
	// its coder's pull request) as long as the child's repositories are
	// within the parent's own; a child of somebody else may not.
	child := task.DeepCopy()
	child.Name, child.ResourceVersion, child.UID = "child-task", "", "child-uid"
	child.OwnerReferences = []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: testMyTaskName, UID: "task-uid", Controller: new(true)}}
	if err := k8sClient.Create(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	owned := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: testMyTaskName, TaskUID: "task-uid", LinkedAccounts: linked})
	if _, repo, token, _, err := resolveForgeRepoAndToken(owned, k8sClient, "create_pull_request", "child-task", "", ""); err != nil || repo != "taskrepo" || token != "linked-token" {
		t.Fatalf("owned child: repo=%q token=%q err=%v", repo, token, err)
	}
	stranger := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: testMyTaskName, TaskUID: "another-uid", LinkedAccounts: linked})
	if _, _, _, _, err := resolveForgeRepoAndToken(stranger, k8sClient, "create_pull_request", "child-task", "", ""); err == nil || !strings.Contains(err.Error(), "child tasks") {
		t.Fatalf("foreign child err = %v", err)
	}
	// A child the coordinator pointed at another repository (delegate_task
	// takes model-supplied repositories) never widens the linked scope.
	wide := child.DeepCopy()
	wide.Name, wide.ResourceVersion, wide.UID = "wide-child", "", "wide-uid"
	wide.Spec.Workspace.GitRepo = "https://github.com/taskorg/otherrepo"
	if err := k8sClient.Create(context.Background(), wide); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := resolveForgeRepoAndToken(owned, k8sClient, "create_pull_request", "wide-child", "", ""); err == nil || !strings.Contains(err.Error(), "outside the current task's repository scope") {
		t.Fatalf("wide child err = %v", err)
	}
	// A transaction's repository context never widens a linked token's scope.
	transacted := task.DeepCopy()
	transacted.Name, transacted.ResourceVersion, transacted.UID = "tx-task", "", "tx-uid"
	transacted.Spec.Transaction = &corev1alpha1.TaskTransaction{Context: map[string]string{"repo": "https://github.com/txorg/txrepo"}}
	if err := k8sClient.Create(context.Background(), transacted); err != nil {
		t.Fatal(err)
	}
	txCtx := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: "tx-task", LinkedAccounts: linked})
	if _, _, _, _, err := resolveScopedReadRepoAndToken(txCtx, k8sClient, "list_pull_requests", "", "https://github.com/txorg/txrepo", ""); err == nil {
		t.Fatal("transaction repo context must not scope a linked token")
	}
	txSecret := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: "tx-task", LinkedAccounts: unboundFor(t)})
	// (The scope is accepted; only the credential lookup fails here, as
	// before, because the transaction scope carries no Secret reference.)
	if _, _, _, _, err := resolveScopedReadRepoAndToken(txSecret, k8sClient, "list_pull_requests", "", "https://github.com/txorg/txrepo", ""); err == nil || strings.Contains(err.Error(), "repository scope") {
		t.Fatalf("transaction scope under Task credentials must be unchanged: err=%v", err)
	}
}

// TestResolveRepoAndToken_LinkedAccountChildReadOnceUncached covers a child
// Task deleted and recreated under the same name: a linked call judges the
// child through the uncached reader rather than a stale cache, and scopes
// the token with the very object whose ownership it validated, never a
// second read of the name that may by then be the replacement.
func TestResolveRepoAndToken_LinkedAccountChildReadOnceUncached(t *testing.T) {
	k8sClient, task := linkedAccountFixture(t)
	linked := &fakeLinkedAccounts{credential: LinkedAccountCredential{AccessToken: "linked-token", Provider: "github"}, bound: true}
	child := task.DeepCopy()
	child.Name, child.ResourceVersion, child.UID = "child-task", "", "child-uid"
	child.OwnerReferences = []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: testMyTaskName, UID: "task-uid", Controller: new(true)}}
	if err := k8sClient.Create(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	// The uncached reader sees the replacement: the same name, owned by
	// someone else. The stale client still holds the original child.
	replacement := child.DeepCopy()
	replacement.UID = "replacement-uid"
	replacement.OwnerReferences[0].UID = "someone-else"
	replacement.Spec.Workspace.GitRepo = "https://github.com/elsewhere/repo"
	uncached := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(task.DeepCopy(), replacement).Build()
	ctx := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: testMyTaskName, TaskUID: "task-uid", LinkedAccounts: linked, PolicyReader: uncached})
	if _, _, _, _, err := resolveForgeRepoAndToken(ctx, k8sClient, "create_pull_request", "child-task", "", ""); err == nil || !strings.Contains(err.Error(), "child tasks") {
		t.Fatalf("replaced child through the uncached reader: err = %v, want refusal", err)
	}
	// A child that changes between reads is read only once: the validated
	// object's workspace scopes the call.
	var childReads atomic.Int32
	swapping := interceptor.NewClient(fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(task.DeepCopy(), child.DeepCopy()).Build(), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "child-task" && childReads.Add(1) > 1 {
				replacement.DeepCopyInto(obj.(*corev1alpha1.Task))
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	ctx = WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: testMyTaskName, TaskUID: "task-uid", LinkedAccounts: linked, PolicyReader: swapping})
	if owner, repo, _, _, err := resolveForgeRepoAndToken(ctx, k8sClient, "create_pull_request", "child-task", "", ""); err != nil || owner != "taskorg" || repo != "taskrepo" || childReads.Load() != 1 {
		t.Fatalf("validated child: %s/%s err = %v reads = %d, want its own workspace from one read", owner, repo, err, childReads.Load())
	}
}

func TestResolveRepoAndToken_LinkedAccountFailsClosed(t *testing.T) {
	k8sClient, _ := linkedAccountFixture(t)
	// No link bound: the Task Secret path is unchanged.
	unbound := &fakeLinkedAccounts{}
	ctx := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: testMyTaskName, LinkedAccounts: unbound})
	if _, _, token, _, err := resolveScopedReadRepoAndToken(ctx, k8sClient, "list_pull_requests", "", "", ""); err != nil || token != "task-secret-token" {
		t.Fatalf("unbound: token=%q err=%v", token, err)
	}
	if _, _, _, _, err := resolveForgeRepoAndToken(ctx, k8sClient, "create_pull_request", "", "", ""); err == nil {
		t.Fatal("unbound forge mutation still needs the forge credential")
	}

	// A bound link that cannot be used fails closed: no Secret fallback.
	broken := &fakeLinkedAccounts{err: errors.New("connection revoked")}
	ctx = WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: testMyTaskName, LinkedAccounts: broken})
	if _, _, _, _, err := resolveScopedReadRepoAndToken(ctx, k8sClient, "list_pull_requests", "", "", ""); err == nil || !strings.Contains(err.Error(), "connection revoked") {
		t.Fatalf("broken link err = %v, want fail closed", err)
	}
	empty := &fakeLinkedAccounts{bound: true}
	ctx = WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: testMyTaskName, LinkedAccounts: empty})
	if _, _, _, _, err := resolveScopedReadRepoAndToken(ctx, k8sClient, "list_pull_requests", "", "", ""); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("empty credential err = %v, want fail closed", err)
	}
	if _, _, _, _, err := resolveScopedReadRepoAndToken(ctx, k8sClient, "", "", "", ""); err == nil || !strings.Contains(err.Error(), "tool name") {
		t.Fatalf("missing tool name err = %v", err)
	}
}

//go:fix inline

func unboundFor(t *testing.T) *fakeLinkedAccounts {
	t.Helper()
	return &fakeLinkedAccounts{}
}

func TestReadGitHubResponseRefusesOversizedDocuments(t *testing.T) {
	if data, err := readGitHubResponse(strings.NewReader(`{"ok":true}`), 64); err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("data=%q err=%v", data, err)
	}
	exact := strings.Repeat("x", 64)
	if data, err := readGitHubResponse(strings.NewReader(exact), 64); err != nil || len(data) != 64 {
		t.Fatalf("a document exactly at the limit must be read: len=%d err=%v", len(data), err)
	}
	// One byte over is refused whole rather than decoded cut in half.
	if _, err := readGitHubResponse(strings.NewReader(exact+"y"), 64); err == nil || !strings.Contains(err.Error(), "exceeds 64 bytes") {
		t.Fatalf("oversized err = %v", err)
	}
}

// TestLoadGitHubTaskScopesFencesTheCurrentTaskUID covers a current Task
// deleted and recreated under the same name after the call was
// authenticated: the replacement's workspace never scopes the call.
func TestLoadGitHubTaskScopesFencesTheCurrentTaskUID(t *testing.T) {
	task, secret := githubRepoTaskWithSecret("https://github.com/orka-agents/orka")
	task.UID = "replacement-uid"
	k8sClient := newFakeClient(task, secret)
	ctx := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: task.Name, TaskUID: "authenticated-uid"})
	if _, err := loadGitHubTaskScopes(ctx, k8sClient, "", true); err == nil || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("err = %v, want the replaced current Task refused", err)
	}
	ctx = WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: task.Name, TaskUID: "replacement-uid"})
	scopes, err := loadGitHubTaskScopes(ctx, k8sClient, "", true)
	if err != nil || len(scopes.scopes) == 0 {
		t.Fatalf("scopes = %+v err = %v, want the authenticated Task's workspace", scopes, err)
	}
}
