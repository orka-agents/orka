/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
)

func TestCreateSecurityPullRequest_ExistingPR(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for sozercan:orka/security/fnd-123."}]}`) //nolint:errcheck
		case http.MethodGet:
			require.Equal(t, "sozercan:orka/security/fnd-123", r.URL.Query().Get("head"))
			require.Equal(t, "main", r.URL.Query().Get("base"))
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `[{"html_url":"https://github.com/sozercan/actions-test/pull/99","number":99}]`) //nolint:errcheck
		default:
			t.Fatalf("unexpected method: %s", r.Method)
		}
	}))
	defer server.Close()

	previousBaseURL := githubPullRequestAPIBaseURL
	githubPullRequestAPIBaseURL = server.URL
	t.Cleanup(func() {
		githubPullRequestAPIBaseURL = previousBaseURL
	})

	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scan-1",
			Namespace: "demo",
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/sozercan/actions-test",
			Branch:  "main",
			GitSecretRef: &corev1.LocalObjectReference{
				Name: "git-creds",
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "git-creds",
			Namespace: "demo",
		},
		Data: map[string][]byte{
			"token": []byte("test-token"),
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, secret).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	securityStore := sqlite.NewStore(db, ":memory:")

	handlers := NewHandlers(HandlersConfig{
		Client:        fakeClient,
		SecurityStore: securityStore,
	})

	ctx := context.Background()
	require.NoError(t, securityStore.UpsertFinding(ctx, &store.Finding{
		ID:             "finding-1",
		Namespace:      "demo",
		RepositoryScan: "scan-1",
		ScanRunID:      "scan-run-1",
		Fingerprint:    "fp-1",
		Title:          "Command injection",
		Summary:        "Unsanitized user input reaches shell execution.",
		Severity:       "critical",
		Confidence:     "high",
		State:          "validated",
		RootCause:      "Shell command arguments are concatenated directly.",
		Remediation:    "Use argument arrays and validate inputs.",
	}))
	require.NoError(t, securityStore.CreatePatchProposal(ctx, &store.PatchProposal{
		ID:             "patch-1",
		Namespace:      "demo",
		RepositoryScan: "scan-1",
		FindingID:      "finding-1",
		TaskName:       "patch-task-1",
		Branch:         "orka/security/fnd-123",
		Status:         "succeeded",
	}))

	app := fiber.New()
	app.Post("/security/findings/:id/pull-request", handlers.CreateSecurityPullRequest)

	req := httptest.NewRequest(http.MethodPost, "/security/findings/finding-1/pull-request?namespace=demo", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		PRNumber int    `json:"prNumber"`
		PRURL    string `json:"prURL"`
		Status   string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, 99, result.PRNumber)
	require.Equal(t, "https://github.com/sozercan/actions-test/pull/99", result.PRURL)
	require.Equal(t, "existing", result.Status)

	proposals, err := securityStore.ListPatchProposals(ctx, "demo", "finding-1")
	require.NoError(t, err)
	require.Len(t, proposals, 1)
	require.Equal(t, "pr_opened", proposals[0].Status)
	require.Equal(t, "https://github.com/sozercan/actions-test/pull/99", proposals[0].PRURL)
	require.NotNil(t, proposals[0].PRNumber)
	require.Equal(t, 99, *proposals[0].PRNumber)

	finding, err := securityStore.GetFinding(ctx, "demo", "finding-1")
	require.NoError(t, err)
	require.Equal(t, "pr_open", finding.State)
	require.Equal(t, "https://github.com/sozercan/actions-test/pull/99", finding.PRURL)
	require.NotNil(t, finding.PRNumber)
	require.Equal(t, 99, *finding.PRNumber)
}

func TestCreateSecurityPatchTaskRequiresPushedBranch(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scan-1",
			Namespace: "demo",
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/sozercan/actions-test",
			Branch:  "main",
			GitSecretRef: &corev1.LocalObjectReference{
				Name: "git-creds",
			},
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
			PatchAgentRef:    &corev1alpha1.AgentReference{Name: "patch"},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	securityStore := sqlite.NewStore(db, ":memory:")

	handlers := NewHandlers(HandlersConfig{
		Client:        fakeClient,
		SecurityStore: securityStore,
	})

	finding := &store.Finding{
		ID:         "fnd_123",
		Namespace:  "demo",
		Title:      "Command injection",
		Severity:   "high",
		Confidence: "high",
	}

	proposal, err := handlers.createSecurityPatchTask(context.Background(), scan, finding)
	require.NoError(t, err)
	require.Equal(t, "orka/security/fnd-123", proposal.Branch)

	task := &corev1alpha1.Task{}
	require.NoError(t, fakeClient.Get(context.Background(), clientObjectKey("demo", proposal.TaskName), task))
	require.Equal(t, "true", envValue(task.Spec.Env, "ORKA_REQUIRE_PUSH_BRANCH"))
	require.NotNil(t, task.Spec.AgentRuntime)
	require.NotNil(t, task.Spec.AgentRuntime.Workspace)
	require.Equal(t, proposal.Branch, task.Spec.AgentRuntime.Workspace.PushBranch)
}

func clientObjectKey(namespace, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: namespace, Name: name}
}

func envValue(envs []corev1.EnvVar, name string) string {
	for _, env := range envs {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}
