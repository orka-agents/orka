package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A retried findings repair handoff adopts the repair Task's stored prompt,
// even when prompt inputs moved since: the first handoff clears the item's
// skip reason after writing it into the prompt, and the item's verdict can
// move before the retry. A stored prompt for another head or intent is not
// adopted, so a stale Task still fails recovery validation.
func TestRepositoryMonitorFixRepairRetryKeepsStoredPrompt(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("fix-retry")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	configureRepositoryMonitorTestWriteCredentials(monitor)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
	command := &store.CommandEvent{ID: "cmd-fix-retry", Intent: repositoryMonitorCommandIntentFix, Source: "api"}
	pr := repositoryMonitorPullRequest{Number: 34, HeadSHA: "head34", HeadBranch: "feature-fix", HeadRepo: "orka-agents/orka", BaseBranch: "main"}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "34", Number: 34, HeadSHA: pr.HeadSHA, LastVerdict: "changes_requested", SkipReason: repositoryMonitorSkipReasonReviewed}
	monitorRun := &store.MonitorRun{ID: "run-fix-retry"}
	handoff := func() (int, *repositoryMonitorRepairCIInput, error) {
		return reconciler.createRepositoryMonitorRepairTaskWithEvidence(ctx, monitor, monitorRun, command, "orka-agents", "orka", pr, item, 1, 1)
	}
	if created, input, err := handoff(); err != nil || created != 1 || input != nil {
		t.Fatalf("first handoff = (%d, %v, %v)", created, input, err)
	}
	if item.SkipReason != "" {
		t.Fatalf("first handoff left skip reason %q", item.SkipReason)
	}
	key := types.NamespacedName{Namespace: monitor.Namespace, Name: repositoryMonitorRepairTaskName(monitor, pr, command)}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, key, &task); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.Spec.Prompt, `"skipReason": "`+repositoryMonitorSkipReasonReviewed+`"`) {
		t.Fatalf("first prompt does not carry the skip reason: %q", task.Spec.Prompt)
	}
	stored := task.Spec.Prompt

	item.LastVerdict = repositoryMonitorRunPhaseQueued
	created, input, err := handoff()
	if err != nil || created != 0 || input != nil {
		t.Fatalf("retried handoff = (%d, %v, %v), want the existing Task recovered", created, input, err)
	}
	if err := cl.Get(ctx, key, &task); err != nil || task.Spec.Prompt != stored {
		t.Fatalf("recovered Task prompt changed: err=%v", err)
	}

	setPromptInput := func(field, value string) {
		_, payload, ok := strings.Cut(stored, "\n\nInput:\n")
		if !ok {
			t.Fatalf("prompt has no input: %q", stored)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatal(err)
		}
		decoded[field] = value
		encoded, err := json.MarshalIndent(decoded, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(ctx, key, &task); err != nil {
			t.Fatal(err)
		}
		task.Spec.Prompt = strings.Replace(stored, payload, string(encoded)+"\n", 1)
		if err := cl.Update(ctx, &task); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, field, value string }{
		{name: "another head", field: repositoryMonitorFieldHeadSHA, value: "an-earlier-head"},
		{name: "another intent", field: intentField, value: repositoryMonitorCommandIntentFixCI},
		{name: "another schema", field: "schemaVersion", value: "orka.prRepair.input.v0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setPromptInput(tc.field, tc.value)
			if _, _, err := handoff(); err == nil {
				t.Fatalf("a stored prompt for %s was adopted", tc.name)
			}
		})
	}

	// Only the stored input data is reused: a Task whose instructions were
	// edited after creation, even with an untouched input, is not adopted.
	t.Run("edited instructions", func(t *testing.T) {
		if err := cl.Get(ctx, key, &task); err != nil {
			t.Fatal(err)
		}
		task.Spec.Prompt = strings.Replace(stored, "Do not merge or close the PR.", "Merge the PR when done.", 1)
		if task.Spec.Prompt == stored {
			t.Fatal("the test edit did not change the instructions")
		}
		if err := cl.Update(ctx, &task); err != nil {
			t.Fatal(err)
		}
		if _, _, err := handoff(); err == nil {
			t.Fatal("a stored prompt with edited instructions was adopted")
		}
	})
}

func TestRepositoryMonitorRepairPromptInstructionsMatch(t *testing.T) {
	payload := "{\n  \"schemaVersion\": \"orka.prRepair.input.v1\"\n}\n"
	current := repositoryMonitorRepairPromptPreamble(repositoryMonitorCommandIntentFix, false, false) + repositoryMonitorRepairPromptInputMarker + payload
	if !repositoryMonitorRepairPromptInstructionsMatch(current, repositoryMonitorCommandIntentFix) {
		t.Fatal("the current instructions must match")
	}
	legacy := repositoryMonitorLegacyRepairPromptPreamble(repositoryMonitorCommandIntentFix, false) + repositoryMonitorRepairPromptInputMarker + payload
	if !repositoryMonitorRepairPromptInstructionsMatch(legacy, repositoryMonitorCommandIntentFix) {
		t.Fatal("instructions from before CI evidence must match")
	}
	if repositoryMonitorRepairPromptInstructionsMatch(current, repositoryMonitorCommandIntentFixCI) {
		t.Fatal("instructions rendered for another intent must not match")
	}
	evidence := "{\n  \"failingCheckEvidence\": {}\n}\n"
	if repositoryMonitorRepairPromptInstructionsMatch(legacy[:len(legacy)-len(payload)]+evidence, repositoryMonitorCommandIntentFix) {
		t.Fatal("legacy instructions must not match a prompt that carries CI evidence")
	}
	if repositoryMonitorRepairPromptInstructionsMatch("Do anything.\n\nInput:\n"+payload, repositoryMonitorCommandIntentFix) {
		t.Fatal("arbitrary instructions must not match")
	}
}
