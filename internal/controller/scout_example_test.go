/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	sigsyaml "sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/scout"
	"github.com/orka-agents/orka/internal/tools"
)

// TestScoutExampleIsReadOnlyAndCredentialFree loads the shipped scout example,
// plans its real ACP runtime profile, and checks that the resulting session
// can only read: a read-intent workspace, no shell, no provider-native web or
// write tools, brokered read-only web tools, and no credential reference of
// any kind in the Agent or the Task.
func TestScoutExampleIsReadOnlyAndCredentialFree(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "scout")
	agent := &corev1alpha1.Agent{}
	task := &corev1alpha1.Task{}
	watchlist := &corev1.ConfigMap{}
	decodeScoutExample(t, filepath.Join(root, "agent.yaml"), agent)
	decodeScoutExample(t, filepath.Join(root, "task.yaml"), task)
	decodeScoutExample(t, filepath.Join(root, "watchlist.yaml"), watchlist)

	checkScoutSchedule(t, task)
	checkScoutCredentialFree(t, task, agent)
	systemPrompt := scoutSystemPrompt(t, agent, watchlist)
	// A digest must report exactly the watchlist's sources, so the shipped
	// watchlist has to yield its source ids.
	sourceIDs, err := scout.WatchlistSourceIDs(systemPrompt)
	if err != nil || len(sourceIDs) != 14 {
		t.Fatalf("watchlist source ids = %v, %v; want 14 ids", sourceIDs, err)
	}

	agent.Namespace, agent.UID, agent.Generation = "default", types.UID("scout-agent-uid"), 1
	task = scheduledScoutRun(t, task)
	configuration, err := buildACPAgentSessionConfiguration(task, agent, systemPrompt)
	if err != nil {
		t.Fatalf("build session configuration: %v", err)
	}
	plan, err := PlanACPRuntimeWithConfiguration(task, agent, ACPRuntimeImages{
		Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64),
	}, configuration)
	if err != nil {
		t.Fatalf("plan scout runtime: %v", err)
	}
	if plan.Profile.WorkspaceIntent != harnessv2.WorkspaceIntent(corev1alpha1.WorkspaceIntentRead) {
		t.Fatalf("profile workspace intent = %q, want read", plan.Profile.WorkspaceIntent)
	}

	registry := tools.NewRegistry()
	if err := tools.RegisterBrokeredWebTools(registry); err != nil {
		t.Fatal(err)
	}
	mcp, err := buildRuntimeSessionMCPConfigurationWithRegistry(context.Background(), nil, task, agent, plan.Profile, registry)
	if err != nil {
		t.Fatalf("build scout MCP policy: %v", err)
	}
	checkScoutToolSurface(t, mcp)
}

// scheduledScoutRun returns the child Task the scheduler creates from the
// template, mirroring its labels and annotation copy, because the tool surface
// of a scheduled run differs from that of the template itself.
func scheduledScoutRun(t *testing.T, template *corev1alpha1.Task) *corev1alpha1.Task {
	t.Helper()
	annotations := map[string]string{labels.AnnotationParentTaskName: template.Name}
	if template.Annotations[labels.AnnotationDisableCoordinationToolInject] == scheduledRunLabelValue {
		annotations[labels.AnnotationDisableCoordinationToolInject] = scheduledRunLabelValue
	}
	run := template.DeepCopy()
	run.Name, run.Namespace, run.UID = template.Name+"-1700000000", "default", types.UID("scout-run-uid")
	run.Labels = map[string]string{
		labels.LabelParentTask:   labels.SelectorValue(template.Name),
		labels.LabelScheduledRun: scheduledRunLabelValue,
	}
	run.Annotations = annotations
	run.Spec.Schedule = ""
	run.Spec.TimeZone = nil
	run.Spec.ConcurrencyPolicy = ""
	run.Spec.StartingDeadlineSeconds = nil
	run.Spec.SuccessfulRunsHistoryLimit = nil
	run.Spec.FailedRunsHistoryLimit = nil
	run.Spec.Suspend = nil
	return run
}

// checkScoutSchedule requires a weekly template with no overlapping runs that
// stays off until a person enables it, with wall-clock and turn caps.
func checkScoutSchedule(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	if task.Spec.Schedule == "" || task.Spec.ConcurrencyPolicy != corev1alpha1.ForbidConcurrent {
		t.Fatalf("scout Task must be scheduled with concurrencyPolicy Forbid, got schedule=%q policy=%q", task.Spec.Schedule, task.Spec.ConcurrencyPolicy)
	}
	if task.Spec.Suspend == nil || !*task.Spec.Suspend {
		t.Fatal("scout Task must ship suspended so applying the example starts nothing")
	}
	if task.Spec.Timeout == nil || task.Spec.Timeout.Duration <= 0 {
		t.Fatal("scout Task must set a wall-clock timeout")
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.MaxTurns == nil {
		t.Fatal("scout Task must cap turns")
	}
}

// checkScoutCredentialFree rejects every credential or publication surface on
// the Task and the Agent.
func checkScoutCredentialFree(t *testing.T, task *corev1alpha1.Task, agent *corev1alpha1.Agent) {
	t.Helper()
	workspace := task.Spec.Workspace
	if workspace == nil || workspace.Intent != corev1alpha1.WorkspaceIntentRead || workspace.GitRepo == "" {
		t.Fatalf("scout Task must use a read-intent workspace, got %#v", workspace)
	}
	if workspace.CreatePR || workspace.PushBranch != "" || workspace.PublicationGitRepo != "" ||
		workspace.ReadCredentialRef != nil || workspace.PublicationCredentialRef != nil ||
		workspace.PublicationReadCredentialRef != nil || workspace.ForgeCredentialRef != nil {
		t.Fatalf("scout workspace must carry no credential or publication setting: %#v", workspace)
	}
	if task.Spec.SecretRef != nil || len(task.Spec.Env) != 0 || task.Spec.Transaction != nil {
		t.Fatal("scout Task must not reference Secrets, environment, or transaction credentials")
	}
	if agent.Spec.SecretRef != nil || agent.Spec.ProviderRef != nil || len(agent.Spec.Tools) != 0 ||
		len(agent.Spec.Skills) != 0 || agent.Spec.Coordination != nil {
		t.Fatal("scout Agent must not reference Secrets, providers, custom tools, skills, or coordination")
	}
}

// scoutSystemPrompt resolves the people-owned watchlist the way the
// controller resolves a ConfigMap-backed system prompt.
func scoutSystemPrompt(t *testing.T, agent *corev1alpha1.Agent, watchlist *corev1.ConfigMap) string {
	t.Helper()
	if agent.Spec.SystemPrompt == nil || agent.Spec.SystemPrompt.ConfigMapRef == nil ||
		agent.Spec.SystemPrompt.ConfigMapRef.Name != watchlist.Name {
		t.Fatalf("scout Agent system prompt must come from the watchlist ConfigMap, got %#v", agent.Spec.SystemPrompt)
	}
	key := agent.Spec.SystemPrompt.ConfigMapRef.Key
	systemPrompt, ok := watchlist.Data[key]
	if !ok || strings.TrimSpace(systemPrompt) == "" {
		t.Fatalf("watchlist ConfigMap has no %q key", key)
	}
	return systemPrompt
}

// checkScoutToolSurface requires exactly the read-only checkout tools plus
// the brokered web tools, all read-only, with no shell.
func checkScoutToolSurface(t *testing.T, mcp harnessv2.MCPPolicyConfiguration) {
	t.Helper()
	if mcp.ToolPolicy.AllowBash {
		t.Fatal("scout session must not allow Bash")
	}
	names := make([]string, 0, len(mcp.ToolPolicy.Tools))
	for _, descriptor := range mcp.ToolPolicy.Tools {
		names = append(names, descriptor.Name)
		if descriptor.Effect != harnessv2.MCPToolEffectReadOnly {
			t.Fatalf("scout tool %q effect = %q, want read-only", descriptor.Name, descriptor.Effect)
		}
		if (descriptor.Name == "web_search" || descriptor.Name == "web_fetch") &&
			descriptor.Source != harnessv2.MCPToolSourceBrokeredBuiltin {
			t.Fatalf("scout tool %q source = %q, want brokered built-in", descriptor.Name, descriptor.Source)
		}
	}
	slices.Sort(names)
	if want := []string{"Glob", "Grep", "Read", "web_fetch", "web_search"}; !slices.Equal(names, want) {
		t.Fatalf("scout tools = %v, want %v", names, want)
	}
}

func decodeScoutExample(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sigsyaml.UnmarshalStrict(data, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
