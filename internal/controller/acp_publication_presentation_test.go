package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/publisher"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

func presentationTestPublisher(t *testing.T, supports bool, calls *atomic.Int32) *publisherservice.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != publisherservice.CapabilitiesPath {
			t.Error("unexpected publisher side effect")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(publisherservice.CapabilitiesResponse{
			Protocol: publisherservice.ProtocolVersion, PullRequestReconciliation: true, PullRequestPresentation: supports,
		})
	}))
	t.Cleanup(server.Close)
	client, err := publisherservice.NewClient(publisherservice.ClientConfig{
		BaseURL: server.URL, HTTPClient: server.Client(), BearerToken: []byte(strings.Repeat("a", 32)), CapabilitySecret: []byte(strings.Repeat("b", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestPublisherPresentationCapabilityGatesExplicitOverrides(t *testing.T) {
	for _, supports := range []bool{false, true} {
		for _, field := range []string{"default", "title", "body"} {
			t.Run(field+"/supported="+fmt.Sprint(supports), func(t *testing.T) {
				var calls atomic.Int32
				dispatcher := &ACPDispatcher{Publisher: presentationTestPublisher(t, supports, &calls)}
				task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
					Prompt:    "fix: update the workspace",
					Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite, CreatePR: true},
				}}
				task.Status.Execution = &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "presentation"}
				switch field {
				case "title":
					task.Spec.Workspace.PRTitle = "fix: exact title"
				case "body":
					task.Spec.Workspace.PRBody = "Authored body."
				}
				got, err := dispatcher.pullRequestPresentationCapability(t.Context(), task)
				if !supports && field != "default" {
					if err == nil || !strings.Contains(err.Error(), "does not support") {
						t.Fatal("unsupported explicit override was not rejected")
					}
					// Recheck at delivery before any artifact, claim, or push can be used.
					_, err = dispatcher.publishWorkspaceDelta(t.Context(), task, "", store.ControllerEpochFence{}, harnessv2.WorkspaceBaseline{}, harnessv2.WorkspaceDeltaDescriptor{}, nil)
					if err == nil || !strings.Contains(err.Error(), "does not support") {
						t.Fatal("unsupported presentation was not gated before publication")
					}
				} else if err != nil || got != supports {
					t.Fatalf("capability negotiation = %t, %v", got, err)
				}
			})
		}
	}
}

func TestTaskPresentationRejectsSensitivePromptBeforePublisherCalls(t *testing.T) {
	fakeToken := "g" + "hp_" + strings.Repeat("NOTAREALSECRET", 3)
	for _, line := range []string{"fix: remove " + fakeToken, strings.Repeat("x", publisher.MaxPullRequestTitleLength-3) + " " + fakeToken} {
		var calls atomic.Int32
		dispatcher := &ACPDispatcher{Publisher: presentationTestPublisher(t, true, &calls)}
		task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Prompt: line + "\nDetails", Workspace: &corev1alpha1.WorkspaceConfig{
			Intent: corev1alpha1.WorkspaceIntentWrite, CreatePR: true,
		}}}
		task.Status.Execution = &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "presentation"}
		if err := validateACPWorkspacePreflight(task); err == nil || !strings.Contains(err.Error(), "must not contain credentials") || strings.Contains(err.Error(), fakeToken) {
			t.Fatal("sensitive prompt did not fail static preflight without echoing the value")
		}
		_, err := dispatcher.publishWorkspaceDelta(context.Background(), task, "", store.ControllerEpochFence{}, harnessv2.WorkspaceBaseline{}, harnessv2.WorkspaceDeltaDescriptor{}, nil)
		if err == nil || !strings.Contains(err.Error(), "must not contain credentials") || strings.Contains(err.Error(), fakeToken) || calls.Load() != 0 {
			t.Fatal("sensitive prompt was not stopped before publisher calls")
		}
		// A trusted explicit title means the prompt line is not published.
		task.Spec.Workspace.PRTitle = "fix: remove credential"
		if err := validateTaskPullRequestText(task); err != nil {
			t.Fatal("explicit safe title did not replace the prompt-derived title")
		}
	}
}
