package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRepositoryMonitorHandlersScopeImplementationLabelOverlap(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		for _, tc := range []struct {
			name, label    string
			protect, pause []string
			issues, labels bool
			valid          bool
		}{
			{name: "PR-only default protected label", protect: []string{"orka:implement"}, labels: true, valid: true},
			{name: "PR-only custom protected label", label: "ship", protect: []string{"SHIP"}, labels: true, valid: true},
			{name: "labels disabled default protected label", protect: []string{"orka:implement"}, issues: true, valid: true},
			{name: "labels disabled custom pause label", label: "ship", pause: []string{"SHIP"}, issues: true, valid: true},
			{name: "active default protected label", protect: []string{" ORKA:IMPLEMENT "}, issues: true, labels: true},
			{name: "active custom protected label", label: "ship", protect: []string{" SHIP "}, issues: true, labels: true},
			{name: "active default pause label", pause: []string{"orka:implement"}, issues: true, labels: true},
			{name: "active custom pause label", label: "ship", pause: []string{"SHIP"}, issues: true, labels: true},
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
				spec.Targets.Issues.Enabled = tc.issues
				spec.Triggers.GitHub.Labels.Enabled = tc.labels
				spec.Triggers.GitHub.Labels.Issues.Implement = tc.label
				spec.Policy.ProtectedLabels, spec.Policy.PauseLabels = tc.protect, tc.pause
				body, err := json.Marshal(CreateRepositoryMonitorRequest{Name: "repo-monitor", Namespace: "demo", Spec: spec})
				require.NoError(t, err)
				req := httptest.NewRequest(method, path, strings.NewReader(string(body)))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				require.NoError(t, err)
				if !tc.valid {
					require.Equal(t, http.StatusBadRequest, resp.StatusCode)
					require.Contains(t, readRespBody(t, resp), "implementation label must not also be a pause or protected label")
				} else if method == http.MethodPost {
					require.Equal(t, http.StatusCreated, resp.StatusCode)
				} else {
					require.Equal(t, http.StatusOK, resp.StatusCode)
				}
			})
		}
	}
}
