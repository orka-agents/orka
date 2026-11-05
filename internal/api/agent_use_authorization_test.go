package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func enableAgentUseAuthorization(t *testing.T) {
	t.Helper()
	previous := agentUseAuthorization.Load()
	agentUseAuthorization.Store(true)
	t.Cleanup(func() { agentUseAuthorization.Store(previous) })
}

// agentUseClientset allows `use` on the listed namespace/name Agents, allows
// every other review, and records the Agent use reviews it saw.
func agentUseClientset(allowed ...string) (*kubefake.Clientset, *[]authorizationv1.ResourceAttributes) {
	grants := map[string]bool{}
	for _, key := range allowed {
		grants[key] = true
	}
	seen := &[]authorizationv1.ResourceAttributes{}
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attributes := review.Spec.ResourceAttributes
		if attributes != nil && attributes.Verb == agentUseVerb {
			*seen = append(*seen, *attributes)
			review.Status.Allowed = grants[attributes.Namespace+"/"+attributes.Name]
			return true, review, nil
		}
		review.Status.Allowed = true
		return true, review, nil
	})
	return clientset, seen
}

func agentUseTask(agentRef *corev1alpha1.AgentReference) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "work"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: agentRef, Prompt: "hi"},
	}
}

func requireForbidden(t *testing.T, err error) {
	t.Helper()
	var fiberErr *fiber.Error
	require.ErrorAs(t, err, &fiberErr)
	require.Equal(t, http.StatusForbidden, fiberErr.Code)
}

func TestAgentUseAuthorizationIsOffByDefault(t *testing.T) {
	clientset, seen := agentUseClientset()
	user := &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview}
	err := authorizeKubernetesTaskCreate(context.Background(), clientset, user, agentUseTask(&corev1alpha1.AgentReference{Name: "reviewer"}))
	require.NoError(t, err)
	require.Empty(t, *seen, "no Agent use review runs while the feature is off")
}

func TestTaskCreateRequiresAgentUse(t *testing.T) {
	enableAgentUseAuthorization(t)
	user := &UserInfo{Username: "alice", Groups: []string{"team-a"}, AuthType: AuthTypeTokenReview}

	denied, seen := agentUseClientset()
	err := authorizeKubernetesTaskCreate(context.Background(), denied, user, agentUseTask(&corev1alpha1.AgentReference{Name: "reviewer"}))
	requireForbidden(t, err)
	require.Len(t, *seen, 1)
	require.Equal(t, authorizationv1.ResourceAttributes{
		Namespace: "team-a", Verb: "use", Group: "core.orka.ai", Version: "v1alpha1", Resource: "agents", Name: "reviewer",
	}, (*seen)[0])

	allowed, _ := agentUseClientset("team-a/reviewer")
	require.NoError(t, authorizeKubernetesTaskCreate(context.Background(), allowed, user,
		agentUseTask(&corev1alpha1.AgentReference{Name: "reviewer"})))

	// A Task without an Agent needs no grant.
	require.NoError(t, authorizeKubernetesTaskCreate(context.Background(), denied, user, agentUseTask(nil)))
}

func TestTaskCreateChecksAgentUseInTheAgentNamespace(t *testing.T) {
	enableAgentUseAuthorization(t)
	clientset, seen := agentUseClientset("agents-team/reviewer")
	user := &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview}
	require.NoError(t, authorizeKubernetesTaskCreate(context.Background(), clientset, user,
		agentUseTask(&corev1alpha1.AgentReference{Name: "reviewer", Namespace: "agents-team"})))
	require.Equal(t, "agents-team", (*seen)[0].Namespace)
}

func TestAgentUseFailsClosedWithoutAnIdentityOrClient(t *testing.T) {
	enableAgentUseAuthorization(t)
	task := agentUseTask(&corev1alpha1.AgentReference{Name: "reviewer"})
	requireForbidden(t, authorizeTaskAgentUse(context.Background(), nil, &UserInfo{Username: "alice"}, task))
	clientset, _ := agentUseClientset("team-a/reviewer")
	requireForbidden(t, authorizeTaskAgentUse(context.Background(), clientset, nil, task))
}

func TestAgentRefsUseChecksOnlyNewlyNamedAgents(t *testing.T) {
	enableAgentUseAuthorization(t)
	clientset, seen := agentUseClientset("team-a/repairer")
	user := &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview}
	previous := repositoryMonitorSpecAgentRefs(corev1alpha1.RepositoryMonitorSpec{
		Agents: corev1alpha1.RepositoryMonitorAgents{Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"}},
	})
	next := repositoryMonitorSpecAgentRefs(corev1alpha1.RepositoryMonitorSpec{
		Agents: corev1alpha1.RepositoryMonitorAgents{
			Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			Repairer: &corev1alpha1.AgentReference{Name: "repairer"},
			Planner:  &corev1alpha1.AgentReference{Name: "repairer"},
		},
	})
	require.NoError(t, authorizeAgentRefsUse(context.Background(), clientset, user, "team-a", "repositorymonitor", "watch", next, previous))
	require.Len(t, *seen, 1, "the kept reviewer is not rechecked and a repeated Agent is checked once")
	require.Equal(t, "repairer", (*seen)[0].Name)

	scanRefs := repositoryScanSpecAgentRefs(corev1alpha1.RepositoryScanSpec{
		AnalysisAgentRef: corev1alpha1.AgentReference{Name: "repairer"},
		PatchAgentRef:    &corev1alpha1.AgentReference{Name: "patcher"},
	})
	requireForbidden(t, authorizeAgentRefsUse(context.Background(), clientset, user, "team-a", "repositoryscan", "scan", scanRefs, nil))
}

func TestCreateRepositoryMonitorEnforcesAgentUse(t *testing.T) {
	create := func(t *testing.T, name string) int {
		t.Helper()
		app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
		body := fmt.Sprintf(`{"name":%q,"namespace":"demo","spec":{"repoURL":%q,"agents":{"reviewer":{"name":"reviewer"}}}}`,
			name, monitorTestRepoURL)
		req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		return resp.StatusCode
	}
	require.Equal(t, http.StatusCreated, create(t, "allowed-when-off"))
	enableAgentUseAuthorization(t)
	require.Equal(t, http.StatusForbidden, create(t, "denied-when-on"),
		"a caller with no verifiable identity cannot name an Agent once use is enforced")
}
