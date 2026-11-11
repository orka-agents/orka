package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/store"
)

type fakeAgentPromoter struct {
	calls  []string
	refuse string
}

func (f *fakeAgentPromoter) PromoteAgent(_ context.Context, namespace, agent, decisionID string,
	actor store.EvalDecisionActor) (*store.AgentPromotionRecord, error) {
	if f.refuse != "" {
		return nil, &evals.RolloutRefusal{Reason: f.refuse}
	}
	f.calls = append(f.calls, "promote "+namespace+"/"+agent+" "+decisionID+" by "+actor.Username)
	return &store.AgentPromotionRecord{ID: "p1", Agent: agent, Action: store.AgentPromotionPromote, State: store.AgentPromotionComplete}, nil
}

func (f *fakeAgentPromoter) RollbackAgent(_ context.Context, namespace, agent string, actor store.EvalDecisionActor,
	reason string) (*store.AgentPromotionRecord, error) {
	if f.refuse != "" {
		return nil, &evals.RolloutRefusal{Reason: f.refuse}
	}
	f.calls = append(f.calls, "rollback "+namespace+"/"+agent+" by "+actor.Username+": "+reason)
	return &store.AgentPromotionRecord{ID: "r1", Agent: agent, Action: store.AgentPromotionRollback, State: store.AgentPromotionComplete}, nil
}

func (f *fakeAgentPromoter) CanaryAgent(_ context.Context, namespace, agent, decisionID string, percent int32,
	actor store.EvalDecisionActor) (*store.AgentPromotionRecord, error) {
	if f.refuse != "" {
		return nil, &evals.RolloutRefusal{Reason: f.refuse}
	}
	f.calls = append(f.calls, fmt.Sprintf("canary %s/%s %s %d%% by %s", namespace, agent, decisionID, percent, actor.Username))
	return &store.AgentPromotionRecord{ID: "c1", Agent: agent, Action: store.AgentPromotionCanary, Percent: percent}, nil
}

func (f *fakeAgentPromoter) AbortAgentRollout(_ context.Context, namespace, agent string, actor store.EvalDecisionActor,
	reason string) (*store.AgentPromotionRecord, error) {
	f.calls = append(f.calls, "abort "+namespace+"/"+agent+" by "+actor.Username+": "+reason)
	return &store.AgentPromotionRecord{ID: "a1", Agent: agent, Action: store.AgentPromotionAbort}, nil
}

func (f *fakeAgentPromoter) AgentRollout(context.Context, string, string) (*corev1alpha1.AgentRolloutStatus, string, error) {
	return &corev1alpha1.AgentRolloutStatus{Candidate: &corev1alpha1.EvalObjectIdentity{Name: "reviewer-c-1"}, Percent: 10,
		DecisionID: "d1", RecordID: "c1"}, "the approval is stale", nil
}

func (f *fakeAgentPromoter) ListAgentPromotions(_ context.Context, _, agent string) ([]store.AgentPromotionRecord, error) {
	if agent != "reviewer" {
		return nil, &evals.RolloutRefusal{Reason: "Agent \"" + agent + "\" does not exist in demo"}
	}
	return []store.AgentPromotionRecord{{ID: "p1", Agent: agent, Action: store.AgentPromotionPromote,
		Previous: store.AgentPromotionValues{SystemPrompt: "secret from a ConfigMap", Model: "m1"},
		Target:   store.AgentPromotionValues{SystemPrompt: "new prompt", Model: "m2"}}}, nil
}

// setupAgentRolloutHandlers grants promote on agents in "demo" to everyone
// but carol.
func setupAgentRolloutHandlers(t *testing.T, promoter *fakeAgentPromoter) *fiber.App {
	t.Helper()
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attrs := review.Spec.ResourceAttributes
		review.Status.Allowed = review.Spec.User != "carol" && attrs != nil && attrs.Verb == "promote" &&
			attrs.Resource == "agents" && attrs.Namespace == "demo" && attrs.Name == "reviewer"
		return true, review, nil
	})
	handlers := NewHandlers(HandlersConfig{KubeClient: clientset, AgentPromoter: promoter, EvalPromoterGroups: []string{"promoters"}})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if user := c.Get("X-Test-User"); user != "" {
			c.Locals(UserInfoContextKey, &UserInfo{Username: user, Groups: strings.Split(c.Get("X-Test-Groups"), ","), AuthType: AuthTypeOIDC})
		}
		return c.Next()
	})
	app.Post("/agents/:name/rollout", handlers.RolloutAgent)
	app.Get("/agents/:name/promotions", handlers.GetAgentPromotions)
	return app
}

func rolloutRequest(t *testing.T, app *fiber.App, user, groups, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agents/reviewer/rollout?namespace=demo", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", user)
	req.Header.Set("X-Test-Groups", groups)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(data)
}

// Only a person in a promoter group with the promote verb on the Agent may
// promote or roll back, and a controller refusal reaches them as a conflict.
func TestRolloutAgent(t *testing.T) {
	const promote = `{"action":"promote","decisionID":"d1"}`
	for name, test := range map[string]struct {
		user, groups, body, refuse string
		wantStatus                 int
		wantBody, wantCall         string
	}{
		"service account":   {user: "system:serviceaccount:demo:bot", groups: "promoters", body: promote, wantStatus: http.StatusForbidden, wantBody: "not a ServiceAccount"},
		"outside the group": {user: "alice", groups: "developers", body: promote, wantStatus: http.StatusForbidden, wantBody: "promoter group"},
		"no promote grant":  {user: "carol", groups: "promoters", body: promote, wantStatus: http.StatusForbidden, wantBody: "promote verb"},
		"unknown action":    {user: "alice", groups: "promoters", body: `{"action":"widen"}`, wantStatus: http.StatusBadRequest, wantBody: "promote, rollback, canary or abort"},
		"canary without a percent": {user: "alice", groups: "promoters", body: `{"action":"canary","decisionID":"d1"}`,
			wantStatus: http.StatusBadRequest, wantBody: "percent from 1 to 100"},
		"canary": {user: "alice", groups: "promoters", body: `{"action":"canary","decisionID":"d1","percent":10}`, wantStatus: http.StatusCreated,
			wantBody: `"id":"c1"`, wantCall: "canary demo/reviewer d1 10% by alice"},
		"abort": {user: "alice", groups: "promoters", body: `{"action":"abort","reason":"noisy"}`, wantStatus: http.StatusCreated,
			wantBody: `"id":"a1"`, wantCall: "abort demo/reviewer by alice: noisy"},
		"promote without a decision": {user: "alice", groups: "promoters", body: `{"action":"promote"}`,
			wantStatus: http.StatusBadRequest, wantBody: "decisionID is required"},
		"rollback with a decision": {user: "alice", groups: "promoters", body: `{"action":"rollback","decisionID":"d1"}`,
			wantStatus: http.StatusBadRequest, wantBody: "takes no decisionID"},
		"refused": {user: "alice", groups: "promoters", body: promote, refuse: "the approval of evalrun/candidate is stale",
			wantStatus: http.StatusConflict, wantBody: "is stale"},
		"promote": {user: "alice", groups: "promoters", body: promote, wantStatus: http.StatusCreated, wantBody: `"id":"p1"`,
			wantCall: "promote demo/reviewer d1 by alice"},
		"rollback": {user: "alice", groups: "promoters", body: `{"action":"rollback","reason":" regressed "}`, wantStatus: http.StatusCreated,
			wantBody: `"id":"r1"`, wantCall: "rollback demo/reviewer by alice: regressed"},
	} {
		t.Run(name, func(t *testing.T) {
			promoter := &fakeAgentPromoter{refuse: test.refuse}
			status, body := rolloutRequest(t, setupAgentRolloutHandlers(t, promoter), test.user, test.groups, test.body)
			require.Equal(t, test.wantStatus, status, body)
			require.Contains(t, body, test.wantBody)
			if test.wantCall != "" {
				require.Equal(t, []string{test.wantCall}, promoter.calls)
			} else {
				require.Empty(t, promoter.calls)
			}
		})
	}
}

func TestGetAgentPromotions(t *testing.T) {
	app := setupAgentRolloutHandlers(t, &fakeAgentPromoter{})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/agents/reviewer/promotions?namespace=demo", nil))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "secret from a ConfigMap", "prompt text is never returned")
	require.NotContains(t, string(raw), "new prompt")
	var page struct {
		Items []AgentPromotionResponse `json:"items"`
	}
	require.NoError(t, json.Unmarshal(raw, &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "m1", page.Items[0].Previous.Model)
	require.Equal(t, len("secret from a ConfigMap"), page.Items[0].Previous.SystemPromptBytes)
	require.Contains(t, page.Items[0].Previous.SystemPromptDigest, "sha256:")
	require.Contains(t, string(raw), `"rollout":{"candidate":{"name":"reviewer-c-1"},"percent":10`)
	require.Contains(t, string(raw), `"rolloutInvalid":"the approval is stale"`)

	missing, err := app.Test(httptest.NewRequest(http.MethodGet, "/agents/nope/promotions?namespace=demo", nil))
	require.NoError(t, err)
	defer func() { _ = missing.Body.Close() }()
	require.Equal(t, http.StatusNotFound, missing.StatusCode)
}
