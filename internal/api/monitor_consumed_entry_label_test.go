package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRepositoryMonitorHandlersValidateConsumedEntryLabelScope(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		for _, tc := range []struct {
			name, label             string
			includes                []string
			consume, labels, issues bool
			valid                   bool
		}{
			{name: "default entry", includes: []string{" ORKA:IMPLEMENT "}, consume: true, labels: true, issues: true},
			{name: "custom entry", label: "ship", includes: []string{"ready", "SHIP"}, consume: true, labels: true, issues: true},
			{name: "retained entry", includes: []string{"orka:implement"}, labels: true, issues: true, valid: true},
			{name: "other scope label", includes: []string{"ready"}, consume: true, labels: true, issues: true, valid: true},
			{name: "command labels disabled", includes: []string{"orka:implement"}, consume: true, issues: true, valid: true},
			{name: "issues disabled", includes: []string{"orka:implement"}, consume: true, labels: true, valid: true},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "forge", Namespace: "demo"}, Data: map[string][]byte{"token": []byte("test-token")}}
				app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff, secret)
				path := "/monitors/repositories"
				if method == http.MethodPut {
					createRepositoryMonitorForHandlerTest(t, app)
					path += "/repo-monitor?namespace=demo"
				}
				spec := corev1alpha1.RepositoryMonitorSpec{RepoURL: monitorTestRepoURL, ForgeCredentialRef: &corev1.LocalObjectReference{Name: secret.Name}, Agents: corev1alpha1.RepositoryMonitorAgents{Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"}}}
				spec.Targets.Issues.Enabled, spec.Targets.Issues.IncludeLabels = tc.issues, tc.includes
				spec.Triggers.GitHub.Labels.Enabled = tc.labels
				spec.Triggers.GitHub.Labels.ConsumeCommandLabels = tc.consume
				spec.Triggers.GitHub.Labels.Issues.Implement = tc.label
				body, err := json.Marshal(CreateRepositoryMonitorRequest{Name: "repo-monitor", Namespace: "demo", Spec: spec})
				require.NoError(t, err)
				req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				require.NoError(t, err)
				if !tc.valid {
					require.Equal(t, http.StatusBadRequest, resp.StatusCode)
					require.Contains(t, readRespBody(t, resp), "includeLabels must not contain the implementation label")
				} else if method == http.MethodPost {
					require.Equal(t, http.StatusCreated, resp.StatusCode)
				} else {
					require.Equal(t, http.StatusOK, resp.StatusCode)
				}
			})
		}
	}
}
