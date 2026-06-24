/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package harness

import "testing"

const (
	runtimeIdentityNamespace = "identity-ns"
	runtimeIdentityTask      = "identity-task"
	runtimeIdentityAgent     = "identity-agent"
)

func TestResolveRuntimeSessionIdentityDefaultsNilLikeInput(t *testing.T) {
	identity := ResolveRuntimeSessionIdentity(RuntimeSessionIdentityInput{RuntimeName: " codex "})
	if identity.ID != "default:default:codex" {
		t.Fatalf("ID = %q, want default:default:codex", identity.ID)
	}
	if identity.Owner.Namespace != "default" || identity.Owner.SessionName != "default" || identity.Owner.ActiveTask != "" {
		t.Fatalf("Owner = %#v, want default owner without active task", identity.Owner)
	}
}

func TestResolveRuntimeSessionIdentityTaskScopedUsesUID(t *testing.T) {
	identity := ResolveRuntimeSessionIdentity(RuntimeSessionIdentityInput{
		Namespace:   runtimeIdentityNamespace,
		TaskName:    runtimeIdentityTask,
		TaskUID:     "uid-a",
		RuntimeName: "codex",
		AgentName:   runtimeIdentityAgent,
		Provider:    ProviderKindKubernetesService,
	})
	if identity.ID != RuntimeSessionID(runtimeIdentityNamespace+":"+runtimeIdentityTask+":uid-a:codex") {
		t.Fatalf("ID = %q, want task-scoped UID identity", identity.ID)
	}
	if identity.Owner.Namespace != runtimeIdentityNamespace || identity.Owner.SessionName != runtimeIdentityTask+":uid-a" || identity.Owner.ActiveTask != runtimeIdentityTask || identity.Owner.AgentName != runtimeIdentityAgent || identity.Owner.Provider != ProviderKindKubernetesService {
		t.Fatalf("Owner = %#v, want task-scoped owner metadata", identity.Owner)
	}
}

func TestResolveRuntimeSessionIdentityExplicitSessionOmitsUID(t *testing.T) {
	identity := ResolveRuntimeSessionIdentity(RuntimeSessionIdentityInput{
		Namespace:   runtimeIdentityNamespace,
		TaskName:    runtimeIdentityTask,
		TaskUID:     "uid-a",
		SessionName: "shared-session",
		RuntimeName: "claude",
		Provider:    ProviderKindKubernetesService,
	})
	if identity.ID != RuntimeSessionID(runtimeIdentityNamespace+":shared-session:claude") {
		t.Fatalf("ID = %q, want explicit session identity", identity.ID)
	}
	if identity.Owner.SessionName != "shared-session" || identity.Owner.ActiveTask != runtimeIdentityTask {
		t.Fatalf("Owner = %#v, want explicit session owner with active task", identity.Owner)
	}
}

func TestResolveRuntimeSessionIdentityFallsBackToTaskNameWithoutUID(t *testing.T) {
	identity := ResolveRuntimeSessionIdentity(RuntimeSessionIdentityInput{
		Namespace:   runtimeIdentityNamespace,
		TaskName:    runtimeIdentityTask,
		RuntimeName: "copilot",
	})
	if identity.ID != RuntimeSessionID(runtimeIdentityNamespace+":"+runtimeIdentityTask+":"+runtimeIdentityTask+":copilot") {
		t.Fatalf("ID = %q, want task-name fallback identity", identity.ID)
	}
}

func TestResolveRuntimeSessionIdentityTrimsInputsAndAllowsExplicitActiveTask(t *testing.T) {
	identity := ResolveRuntimeSessionIdentity(RuntimeSessionIdentityInput{
		Namespace:   " " + runtimeIdentityNamespace + " ",
		TaskName:    " " + runtimeIdentityTask + " ",
		TaskUID:     " uid-a ",
		RuntimeName: " codex ",
		ActiveTask:  " task-b ",
		AgentName:   " " + runtimeIdentityAgent + " ",
		Provider:    " kubernetes-service ",
	})
	if identity.ID != RuntimeSessionID(runtimeIdentityNamespace+":"+runtimeIdentityTask+":uid-a:codex") {
		t.Fatalf("ID = %q, want trimmed identity", identity.ID)
	}
	if identity.Owner.ActiveTask != "task-b" || identity.Owner.AgentName != runtimeIdentityAgent || identity.Owner.Provider != ProviderKindKubernetesService {
		t.Fatalf("Owner = %#v, want trimmed owner", identity.Owner)
	}
}
