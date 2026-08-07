/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

func TestEnsureHarnessV1ExecutionBindingFreezesPublicReadOnlyWorkspace(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	policy := &corev1alpha1.AgentExecutionPolicy{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.policy), policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.AllowPublicReadOnlyWorkspaces = true
	policy.Generation++
	if err := fixture.reconciler.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.task), task); err != nil {
		t.Fatal(err)
	}
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
		Intent:  corev1alpha1.WorkspaceIntentRead,
		GitRepo: "https://github.com/orka-agents/orka.git",
		Branch:  "main",
		Ref:     "0123456789abcdef0123456789abcdef01234567",
		SubPath: "website/docs",
	}
	task.Generation++
	if err := fixture.reconciler.Update(ctx, task); err != nil {
		t.Fatal(err)
	}

	_, err, handled := fixture.reconciler.ensureHarnessV1ExecutionBinding(ctx, task, fixture.agent)
	if err != nil || handled {
		t.Fatalf("ensure public workspace binding = handled=%v err=%v", handled, err)
	}
	bound := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(task), bound); err != nil {
		t.Fatal(err)
	}
	binding := bound.Status.AgentExecutionBinding
	if binding == nil {
		t.Fatal("public read-only workspace binding was not persisted")
	}
	snapshot, err := fixture.reconciler.AgentExecutionSnapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{
		TaskUID: string(bound.UID),
		Digest:  binding.Snapshot.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	var body agentExecutionSnapshotBody
	if err := json.Unmarshal(snapshot.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Workspace == nil || body.Workspace.GitRepo != task.Spec.Workspace.GitRepo ||
		body.Workspace.Branch != task.Spec.Workspace.Branch || body.Workspace.Ref != task.Spec.Workspace.Ref ||
		body.Workspace.SubPath != task.Spec.Workspace.SubPath {
		t.Fatalf("frozen public workspace = %#v, want %#v", body.Workspace, task.Spec.Workspace)
	}
}

func TestValidateHarnessV1PublicReadOnlyWorkspace(t *testing.T) {
	valid := &corev1alpha1.WorkspaceConfig{
		Intent:  corev1alpha1.WorkspaceIntentRead,
		GitRepo: "https://github.com/orka-agents/orka.git",
		Branch:  "main",
		SubPath: "website/docs",
	}
	if err := validateHarnessV1PublicReadOnlyWorkspace(valid); err != nil {
		t.Fatalf("valid public read-only workspace rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*corev1alpha1.WorkspaceConfig)
		want   string
	}{
		{
			name: "write intent",
			mutate: func(workspace *corev1alpha1.WorkspaceConfig) {
				workspace.Intent = corev1alpha1.WorkspaceIntentWrite
			},
			want: "read intent",
		},
		{
			name: "read credential",
			mutate: func(workspace *corev1alpha1.WorkspaceConfig) {
				workspace.ReadCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: "git-read"}
			},
			want: "credential reference",
		},
		{
			name: "publication target",
			mutate: func(workspace *corev1alpha1.WorkspaceConfig) {
				workspace.PublicationGitRepo = "https://github.com/orka-agents/orka-fork.git"
			},
			want: "publication",
		},
		{
			name: "unsafe ref",
			mutate: func(workspace *corev1alpha1.WorkspaceConfig) {
				workspace.Ref = "refs/heads/main..other"
			},
			want: "ref",
		},
		{
			name: "escaping subpath",
			mutate: func(workspace *corev1alpha1.WorkspaceConfig) {
				workspace.SubPath = "../private"
			},
			want: "subPath",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := valid.DeepCopy()
			test.mutate(workspace)
			err := validateHarnessV1PublicReadOnlyWorkspace(workspace)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validate workspace error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveHarnessV1PublicReadOnlyWorkspaceRequiresPolicyAndSupportedRuntime(t *testing.T) {
	workspace := &corev1alpha1.WorkspaceConfig{
		Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: "https://github.com/orka-agents/orka.git",
	}
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Workspace: workspace}}
	agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
		Type: corev1alpha1.AgentRuntimeCodex,
	}}}
	policy := &corev1alpha1.AgentExecutionPolicy{Spec: corev1alpha1.AgentExecutionPolicySpec{
		AllowPublicReadOnlyWorkspaces: true,
		NetworkIsolationProfile:       corev1alpha1.AgentExecutionNetworkIsolationDefaultDeny,
	}}
	target := resolvedHarnessV1Target{backend: corev1alpha1.AgentExecutionBackendHarnessWrapper}
	resolved, err := resolveHarnessV1PublicReadOnlyWorkspace(task, agent, policy, target)
	if err != nil || resolved == nil || resolved == workspace {
		t.Fatalf("resolved workspace = %#v, err=%v; want independent frozen copy", resolved, err)
	}

	disabled := policy.DeepCopy()
	disabled.Spec.AllowPublicReadOnlyWorkspaces = false
	if _, err := resolveHarnessV1PublicReadOnlyWorkspace(task, agent, disabled, target); err == nil ||
		!strings.Contains(err.Error(), "does not authorize") {
		t.Fatalf("disabled policy error = %v", err)
	}
	claude := agent.DeepCopy()
	claude.Spec.Runtime.Type = corev1alpha1.AgentRuntimeClaude
	if _, err := resolveHarnessV1PublicReadOnlyWorkspace(task, claude, policy, target); err == nil ||
		!strings.Contains(err.Error(), "does not support runtime") {
		t.Fatalf("unsupported runtime error = %v", err)
	}
	perTrustDomain := policy.DeepCopy()
	perTrustDomain.Spec.NetworkIsolationProfile = corev1alpha1.AgentExecutionNetworkIsolationPerTrustDomain
	if _, err := resolveHarnessV1PublicReadOnlyWorkspace(task, agent, perTrustDomain, target); err == nil ||
		!strings.Contains(err.Error(), "trust-domain-specific wrapper target") {
		t.Fatalf("per-trust-domain isolation error = %v", err)
	}
	external := target
	external.backend = corev1alpha1.AgentExecutionBackendExternalEndpoint
	external.runtimeRef = &corev1alpha1.AgentRuntime{}
	if _, err := resolveHarnessV1PublicReadOnlyWorkspace(task, agent, policy, external); err == nil ||
		!strings.Contains(err.Error(), "built-in wrapper") {
		t.Fatalf("external runtime error = %v", err)
	}
}

func TestApplyHarnessV1WorkspaceMetadataUsesFrozenCredentialFreeFields(t *testing.T) {
	body := agentExecutionSnapshotBody{
		RuntimeType: string(corev1alpha1.AgentRuntimeCodex),
		Workspace: &corev1alpha1.WorkspaceConfig{
			Intent:  corev1alpha1.WorkspaceIntentRead,
			GitRepo: "https://github.com/orka-agents/orka.git", Branch: "main",
			Ref: "0123456789abcdef0123456789abcdef01234567", SubPath: "website/docs",
		},
		DefaultTools: &agentExecutionSnapshotToolPolicy{
			AllowedTools: []string{}, AllowBash: new(false),
		},
		HarnessV1: &agentExecutionSnapshotHarnessV1{
			Backend: string(corev1alpha1.AgentExecutionBackendHarnessWrapper),
		},
	}
	request := &harness.StartTurnRequest{Metadata: map[string]string{"existing": "value"}}
	if err := applyHarnessV1WorkspaceMetadata(request, body); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"gitRepo":          body.Workspace.GitRepo,
		"gitBranch":        body.Workspace.Branch,
		"gitRef":           body.Workspace.Ref,
		"workspaceSubPath": body.Workspace.SubPath,
		"readOnly":         "true",
		"existing":         "value",
	} {
		if got := request.Metadata[key]; got != want {
			t.Fatalf("request metadata[%q] = %q, want %q", key, got, want)
		}
	}
	unsafe := body
	unsafe.DefaultTools = &agentExecutionSnapshotToolPolicy{
		AllowedTools: []string{}, AllowBash: new(true),
	}
	if err := applyHarnessV1WorkspaceMetadata(&harness.StartTurnRequest{}, unsafe); err == nil ||
		!strings.Contains(err.Error(), "Bash") {
		t.Fatalf("unsafe frozen workspace error = %v", err)
	}

	encoded, err := canonicalAgentExecutionSnapshotBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded agentExecutionSnapshotBody
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Workspace == nil || decoded.Workspace.GitRepo != body.Workspace.GitRepo {
		t.Fatalf("canonical snapshot lost frozen workspace: %#v", decoded.Workspace)
	}
}
