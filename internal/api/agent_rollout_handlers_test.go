package api

import (
	"context"
	"encoding/json"
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

func (f *fakeAgentPromoter) ListAgentPromotions(_ context.Context, _, agent string) ([]store.AgentPromotionRecord, error) {
	if agent != "reviewer" {
		return nil, &evals.RolloutRefusal{Reason: "Agent \"" + agent + "\" does not exist in demo"}
	}
	return []store.AgentPromotionRecord{{ID: "p1", Agent: agent, Action: store.AgentPromotionPromote}}, nil
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
		"unknown action":    {user: "alice", groups: "promoters", body: `{"action":"canary"}`, wantStatus: http.StatusBadRequest, wantBody: "promote or rollback"},
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
	var page struct {
		Items []store.AgentPromotionRecord `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&page))
	require.Len(t, page.Items, 1)

	missing, err := app.Test(httptest.NewRequest(http.MethodGet, "/agents/nope/promotions?namespace=demo", nil))
	require.NoError(t, err)
	defer func() { _ = missing.Body.Close() }()
	require.Equal(t, http.StatusNotFound, missing.StatusCode)
}
