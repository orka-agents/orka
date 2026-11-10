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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
)

type fakeEvalProposer struct {
	started []evals.ProposalRequest
	refuse  string
}

func (f *fakeEvalProposer) StartEvalProposal(_ context.Context, run *corev1alpha1.EvalRun, request evals.ProposalRequest) (*corev1alpha1.Task, error) {
	if f.refuse != "" {
		return nil, &evals.ProposalRefusal{Reason: f.refuse}
	}
	f.started = append(f.started, request)
	return &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "eval-proposal-" + run.Name + "-1", Namespace: run.Namespace}}, nil
}

func (f *fakeEvalProposer) ListEvalProposals(context.Context, *corev1alpha1.EvalRun) ([]evals.Proposal, error) {
	return []evals.Proposal{{Name: "eval-proposal-training-1", State: "Created", CandidateAgent: "reviewer-c-1"}}, nil
}

// setupEvalProposalHandlers serves a training run "training" in "demo".
// RBAC grants propose on evalruns there to everyone but carol.
func setupEvalProposalHandlers(t *testing.T, proposer *fakeEvalProposer) *fiber.App {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	training := &corev1alpha1.EvalRun{ObjectMeta: metav1.ObjectMeta{Name: "training", Namespace: "demo", UID: "training-uid"}}
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attrs := review.Spec.ResourceAttributes
		review.Status.Allowed = review.Spec.User != "carol" && attrs != nil && attrs.Verb == "propose" &&
			attrs.Resource == "evalruns" && attrs.Namespace == "demo"
		return true, review, nil
	})
	handlers := NewHandlers(HandlersConfig{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(training).Build(), KubeClient: clientset,
		EvalProposer: proposer, EvalPromoterGroups: []string{"promoters"},
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if user := c.Get("X-Test-User"); user != "" {
			c.Locals(UserInfoContextKey, &UserInfo{Username: user, Groups: strings.Split(c.Get("X-Test-Groups"), ","), AuthType: AuthTypeOIDC})
		}
		return c.Next()
	})
	app.Post("/evals/runs/:name/proposals", handlers.ProposeEvalRun)
	app.Get("/evals/runs/:name/proposals", handlers.GetEvalRunProposals)
	return app
}

func proposeRequest(t *testing.T, app *fiber.App, user, groups, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/evals/runs/training/proposals?namespace=demo", strings.NewReader(body))
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

// Only a person in a promoter group with the propose verb may ask for a
// proposal, and a controller refusal reaches the caller as a conflict.
func TestProposeEvalRun(t *testing.T) {
	for name, test := range map[string]struct {
		user, groups, body, refuse string
		wantStatus                 int
		wantError                  string
	}{
		"service account":   {user: "system:serviceaccount:demo:bot", groups: "promoters", wantStatus: http.StatusForbidden, wantError: "not a ServiceAccount"},
		"outside the group": {user: "alice", groups: "developers", wantStatus: http.StatusForbidden, wantError: "promoter group"},
		"no propose grant":  {user: "carol", groups: "promoters", wantStatus: http.StatusForbidden, wantError: "propose verb"},
		"target without grader": {user: "alice", groups: "promoters", body: `{"target":{"tag":"defect"}}`,
			wantStatus: http.StatusBadRequest, wantError: "target.grader"},
		"refused": {user: "alice", groups: "promoters", refuse: "evalrun/training has no failed trials to learn from",
			wantStatus: http.StatusConflict, wantError: "no failed trials"},
		"promoter":             {user: "alice", groups: "promoters", wantStatus: http.StatusCreated, wantError: "eval-proposal-training-1"},
		"promoter with target": {user: "alice", groups: "promoters", body: `{"heldOut":"heldout","target":{"grader":"verdict"}}`, wantStatus: http.StatusCreated},
	} {
		t.Run(name, func(t *testing.T) {
			proposer := &fakeEvalProposer{refuse: test.refuse}
			status, body := proposeRequest(t, setupEvalProposalHandlers(t, proposer), test.user, test.groups, test.body)
			require.Equal(t, test.wantStatus, status, body)
			require.Contains(t, body, test.wantError)
			if status == http.StatusCreated {
				require.Len(t, proposer.started, 1)
				require.Equal(t, "alice", proposer.started[0].RequestedBy)
				if test.body != "" {
					require.Equal(t, "heldout", proposer.started[0].HeldOut)
					require.Equal(t, corev1alpha1.EvalGraderVerdict, proposer.started[0].Target.Grader)
				}
			} else {
				require.Empty(t, proposer.started)
			}
		})
	}
}

func TestGetEvalRunProposals(t *testing.T) {
	app := setupEvalProposalHandlers(t, &fakeEvalProposer{})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/evals/runs/training/proposals?namespace=demo", nil))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var page struct {
		Items []evals.Proposal `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "reviewer-c-1", page.Items[0].CandidateAgent)
}
