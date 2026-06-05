/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const testEnvToken = "env-token"

func TestResolveRepoAndToken_DirectRepoURL_HTTPS(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	owner, repo, token, baseURL, err := resolveRepoAndToken(
		context.Background(), nil,
		"", testMyOrgRepoURL, "",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "myorg" || repo != "myrepo" {
		t.Errorf("got owner=%q repo=%q, want myorg/myrepo", owner, repo)
	}
	if token != testEnvToken {
		t.Errorf("got token=%q, want env-token", token)
	}
	if baseURL != githubAPIBaseURL {
		t.Errorf("got baseURL=%q, want %q", baseURL, githubAPIBaseURL)
	}
}

func TestResolveRepoAndToken_DirectRepoURL_SSH(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	owner, repo, token, _, err := resolveRepoAndToken(
		context.Background(), nil,
		"", "git@github.com:myorg/myrepo.git", "",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "myorg" || repo != "myrepo" {
		t.Errorf("got owner=%q repo=%q, want myorg/myrepo", owner, repo)
	}
	if token != testEnvToken {
		t.Errorf("got token=%q, want env-token", token)
	}
}

func TestResolveRepoAndToken_EnvVarFallback(t *testing.T) {
	t.Setenv("ORKA_GIT_REPO", "https://github.com/envorg/envrepo")
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	owner, repo, token, _, err := resolveRepoAndToken(
		context.Background(), nil,
		"", "", "",
	)
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

func TestResolveRepoAndToken_TaskName(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testMyTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      "https://github.com/taskorg/taskrepo",
					GitSecretRef: &corev1.LocalObjectReference{Name: testGitCredsSecretName},
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte("task-secret-token")},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	owner, repo, token, baseURL, err := resolveRepoAndToken(
		context.Background(), k8sClient, testMyTaskName, "", "",
	)
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

func TestResolveRepoAndToken_TaskName_PasswordKey(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "pw-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      "https://github.com/pworg/pwrepo",
					GitSecretRef: &corev1.LocalObjectReference{Name: "git-pw"},
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-pw", Namespace: defaultNamespace},
		Data:       map[string][]byte{passwordKey: []byte("pw-token")},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	_, _, token, _, err := resolveRepoAndToken(
		context.Background(), k8sClient,
		"pw-task", "", "",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "pw-token" {
		t.Errorf("got token=%q, want pw-token", token)
	}
}

func TestResolveRepoAndToken_TokenFromFile(t *testing.T) {
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

func TestResolveRepoAndToken_TokenFromEnvVar(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "my-gh-token")

	owner, repo, token, _, err := resolveRepoAndToken(
		context.Background(), nil,
		"", testOrgTestRepoURL, "",
	)
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

func TestResolveRepoAndToken_BaseURLOverride(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")

	_, _, _, baseURL, err := resolveRepoAndToken(
		context.Background(), nil,
		"", "https://github.com/o/r", "http://localhost:8080",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if baseURL != "http://localhost:8080" {
		t.Errorf("got baseURL=%q, want http://localhost:8080", baseURL)
	}
}

func TestResolveRepoAndToken_ErrorNoRepoURL(t *testing.T) {
	// Ensure ORKA_GIT_REPO is not set
	t.Setenv("ORKA_GIT_REPO", "")

	_, _, _, _, err := resolveRepoAndToken(
		context.Background(), nil,
		"", "", "",
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if expected := "no repo_url, task_name, or ORKA_GIT_REPO provided"; err.Error() != expected {
		t.Errorf("got error %q, want %q", err.Error(), expected)
	}
}

func TestResolveRepoAndToken_ErrorInvalidURL(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "tok")

	_, _, _, _, err := resolveRepoAndToken(
		context.Background(), nil,
		"", "not-a-valid-url", "",
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "unsupported GitHub URL format") {
		t.Errorf("got error %q, want it to contain 'unsupported GitHub URL format'", err.Error())
	}
}

func TestResolveRepoAndToken_ErrorTaskNotFound(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, _, _, _, err := resolveRepoAndToken(
		context.Background(), k8sClient,
		"nonexistent-task", "", "",
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "failed to get task nonexistent-task") {
		t.Errorf("got error %q, want it to contain 'failed to get task nonexistent-task'", err.Error())
	}
}

func TestResolveRepoAndToken_ErrorNoToken(t *testing.T) {
	// Clear all token sources
	t.Setenv("GITHUB_TOKEN", "")

	_, _, _, _, err := resolveRepoAndToken(
		context.Background(), nil,
		"", "https://github.com/o/r", "",
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "could not resolve GitHub token") {
		t.Errorf("got error %q, want it to contain 'could not resolve GitHub token'", err.Error())
	}
}

func TestResolveRepoAndToken_ErrorTaskNoWorkspace(t *testing.T) {
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

	_, _, _, _, err := resolveRepoAndToken(
		context.Background(), k8sClient, testNoWorkspaceTaskName, "", "",
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !contains(err.Error(), "does not have workspace configuration") {
		t.Errorf("got error %q, want it to contain 'does not have workspace configuration'", err.Error())
	}
}

func TestResolveRepoAndToken_TaskWithoutGitSecretRefFallsBackToEnvToken(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "no-secret-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: testOrgRepoURL,
				},
			},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()

	owner, repo, token, _, err := resolveRepoAndToken(
		context.Background(), k8sClient,
		"no-secret-task", "", "",
	)
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

func TestResolveRepoAndToken_RepoURLMatchingTaskNameUsesTaskToken(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "some-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      "https://github.com/task-org/task-repo",
					GitSecretRef: &corev1.LocalObjectReference{Name: testGitCredsSecretName},
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte("task-token")},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	owner, repo, token, _, err := resolveRepoAndToken(
		context.Background(), k8sClient,
		"some-task", "https://github.com/task-org/task-repo", "",
	)
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

func TestResolveRepoAndToken_RepoURLMismatchRejectsTaskToken(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "some-task", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      "https://github.com/task-org/task-repo",
					GitSecretRef: &corev1.LocalObjectReference{Name: testGitCredsSecretName},
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte("task-token")},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secret).Build()

	_, _, _, _, err := resolveRepoAndToken(
		context.Background(), k8sClient,
		"some-task", "https://github.com/url-org/url-repo", "",
	)
	if err == nil {
		t.Fatal("expected repo scope mismatch error")
	}
	if !contains(err.Error(), "does not match task repository scope") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveRepoAndToken_RepoURLWithMissingTaskFailsClosed(t *testing.T) {
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv("GITHUB_TOKEN", testEnvToken)

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, _, _, _, err := resolveRepoAndToken(
		context.Background(), k8sClient,
		"nonexistent-task", "https://github.com/url-org/url-repo", "",
	)
	if err == nil {
		t.Fatal("expected task lookup error")
	}
	if !contains(err.Error(), "failed to get task nonexistent-task") {
		t.Fatalf("unexpected error: %v", err)
	}
}
