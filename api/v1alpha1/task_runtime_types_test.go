/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTaskWorkspaceSchemaFields(t *testing.T) {
	workspace := WorkspaceConfig{
		Intent:  WorkspaceIntentWrite,
		GitRepo: "https://github.com/example/source.git",
		SourceRepository: &RepositoryIdentity{
			Provider: "github",
			ID:       "github.com/example/source",
		},
		Branch: "main",
		Ref:    "0123456789abcdef0123456789abcdef01234567",
		ReadCredentialRef: &WorkspaceCredentialReference{
			Name: "source-read",
		},
		PublicationGitRepo: "https://github.com/example/fork.git",
		PublicationRepository: &RepositoryIdentity{
			Provider: "github",
			ID:       "github.com/example/fork",
		},
		PublicationReadCredentialRef: &WorkspaceCredentialReference{Name: "publication-read"},
		PublicationCredentialRef: &WorkspaceCredentialReference{
			Name: "publication-write",
		},
		ForgeCredentialRef: &WorkspaceCredentialReference{Name: "forge-token"},
		SubPath:            "src/app",
		PRBaseBranch:       "main",
		PushBranch:         "orka/task-full-uid",
		CreatePR:           true,
	}
	task := TaskSpec{Type: TaskTypeAgent, Workspace: &workspace}

	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("json.Marshal(TaskSpec): %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("json.Unmarshal(TaskSpec): %v", err)
	}
	workspaceJSON, ok := fields["workspace"].(map[string]any)
	if !ok {
		t.Fatalf("workspace JSON = %#v, want object", fields["workspace"])
	}
	for _, field := range []string{
		"intent",
		"gitRepo",
		"sourceRepository",
		"readCredentialRef",
		"publicationGitRepo",
		"publicationRepository",
		"publicationReadCredentialRef",
		"publicationCredentialRef",
		"forgeCredentialRef",
		"createPR",
	} {
		if _, ok := workspaceJSON[field]; !ok {
			t.Errorf("workspace JSON missing %q: %s", field, encoded)
		}
	}
	if _, ok := fields["agentRuntime"]; ok {
		t.Fatalf("new top-level workspace unexpectedly serialized agentRuntime: %s", encoded)
	}
}

func TestTaskStructuredExecutionAndDeliveryStatus(t *testing.T) {
	sha := strings.Repeat("a", 40)
	remoteBeforeSHA := sha
	digest := "sha256:" + strings.Repeat("b", 64)
	status := TaskStatus{
		Phase: TaskPhaseSucceeded,
		Execution: &TaskExecutionStatus{
			State:           TaskExecutionStateSucceeded,
			Outcome:         TaskExecutionOutcomeSucceeded,
			Attempt:         1,
			PromptID:        "prompt-1",
			ControllerEpoch: 7,
		},
		Delivery: &TaskDeliveryStatus{
			State:         TaskDeliveryStateVerifiedExact,
			Outcome:       TaskDeliveryOutcomeVerifiedExact,
			PublicationID: "publication-1",
			SourceRepository: &RepositoryIdentity{
				Provider: "github",
				ID:       "github.com/example/source",
			},
			PublicationRepository: &RepositoryIdentity{
				Provider: "github",
				ID:       "github.com/example/fork",
			},
			Branch:            "orka/task-full-uid",
			StartingSHA:       sha,
			RemoteBeforeSHA:   &remoteBeforeSHA,
			TreeSHA:           sha,
			ExpectedCommitSHA: sha,
			VerifiedRemoteSHA: sha,
			ArtifactDigest:    digest,
			PRReceipt: &TaskPullRequestReceipt{
				ID:         "PR_1",
				Number:     42,
				URL:        "https://github.com/example/source/pull/42",
				State:      "open",
				BaseBranch: "main",
				HeadBranch: "orka/task-full-uid",
				HeadSHA:    sha,
			},
		},
	}

	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("json.Marshal(TaskStatus): %v", err)
	}
	var decoded TaskStatus
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(TaskStatus): %v", err)
	}
	if decoded.Execution == nil || decoded.Execution.Outcome != TaskExecutionOutcomeSucceeded {
		t.Fatalf("execution status round trip = %#v", decoded.Execution)
	}
	if decoded.Delivery == nil || decoded.Delivery.Outcome != TaskDeliveryOutcomeVerifiedExact {
		t.Fatalf("delivery status round trip = %#v", decoded.Delivery)
	}
	if decoded.Delivery.PRReceipt == nil || decoded.Delivery.PRReceipt.Number != 42 {
		t.Fatalf("PR receipt round trip = %#v", decoded.Delivery.PRReceipt)
	}
}

func TestTaskExecutionAndDeliveryOutcomeConstants(t *testing.T) {
	execution := map[TaskExecutionOutcome]string{
		TaskExecutionOutcomeSucceeded:      "Succeeded",
		TaskExecutionOutcomeFailed:         "Failed",
		TaskExecutionOutcomeCancelled:      "Cancelled",
		TaskExecutionOutcomeOutcomeUnknown: "OutcomeUnknown",
	}
	for got, want := range execution {
		if string(got) != want {
			t.Errorf("execution outcome = %q, want %q", got, want)
		}
	}

	delivery := map[TaskDeliveryOutcome]string{
		TaskDeliveryOutcomeNotRequested:              "NotRequested",
		TaskDeliveryOutcomeVerifiedExact:             "VerifiedExact",
		TaskDeliveryOutcomeDeliveredSuperseded:       "DeliveredSuperseded",
		TaskDeliveryOutcomeReadValidated:             "ReadValidated",
		TaskDeliveryOutcomeNoChange:                  "NoChange",
		TaskDeliveryOutcomeCancelledBeforePublish:    "CancelledBeforePublish",
		TaskDeliveryOutcomeReadOnlyWorkspaceModified: "ReadOnlyWorkspaceModified",
		TaskDeliveryOutcomeDeliveryConflict:          "DeliveryConflict",
		TaskDeliveryOutcomeCredentialBlocked:         "CredentialBlocked",
		TaskDeliveryOutcomePublicationOutcomeUnknown: "PublicationOutcomeUnknown",
	}
	for got, want := range delivery {
		if string(got) != want {
			t.Errorf("delivery outcome = %q, want %q", got, want)
		}
	}
}

func TestTaskWorkspaceHasNoLegacyCredentialOrForkFields(t *testing.T) {
	workspace := WorkspaceConfig{
		GitRepo:                  "https://github.com/example/source.git",
		ReadCredentialRef:        &WorkspaceCredentialReference{Name: "source-read"},
		PublicationGitRepo:       "https://github.com/example/fork.git",
		PublicationCredentialRef: &WorkspaceCredentialReference{Name: "fork-write"},
	}
	encoded, err := json.Marshal(workspace)
	if err != nil {
		t.Fatalf("json.Marshal(WorkspaceConfig): %v", err)
	}
	if strings.Contains(string(encoded), "gitSecretRef") || strings.Contains(string(encoded), "forkRepo") {
		t.Fatalf("workspace serialized legacy fields: %s", encoded)
	}
}
