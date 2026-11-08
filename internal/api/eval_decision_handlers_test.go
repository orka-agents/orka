package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type fakeEvalDecisionVerifier struct {
	binding *store.EvalDecisionBinding
	err     error
}

func (f *fakeEvalDecisionVerifier) CurrentEvalDecisionBinding(context.Context, *corev1alpha1.EvalRun) (*store.EvalDecisionBinding, error) {
	if f.err != nil {
		return nil, f.err
	}
	binding := *f.binding
	return &binding, nil
}

type evalDecisionTestEnv struct {
	app      *fiber.App
	store    *sqlite.Store
	client   client.Client
	verifier *fakeEvalDecisionVerifier
	binding  store.EvalDecisionBinding
}

func evalDecisionTestBinding() store.EvalDecisionBinding {
	return store.EvalDecisionBinding{
		CandidateAgent:  store.EvalAgentIdentity{Name: "candidate-agent", UID: "agent-uid", Generation: 1},
		CandidateDigest: "sha256:candidate",
		BaselineAgent:   store.EvalAgentIdentity{Name: "reviewer", UID: "reviewer-uid", Generation: 4},
		BaselineDigest:  "sha256:stable",
		MonitorUID:      "monitor-uid", MonitorGeneration: 2,
		CaseSetDigest: "sha256:cases", GraderSetDigest: "sha256:graders", GateDigest: "sha256:gate",
	}
}

// setupEvalDecisionHandlers serves a candidate run "candidate" in "demo"
// whose experiment ended with result, or is undecided when result is empty.
func setupEvalDecisionHandlers(t *testing.T, result string, promoterGroups ...string) *evalDecisionTestEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	candidate := &corev1alpha1.EvalRun{
		ObjectMeta: metav1.ObjectMeta{Name: "candidate", Namespace: "demo", UID: "candidate-uid"},
		Spec: corev1alpha1.EvalRunSpec{Experiment: &corev1alpha1.EvalExperiment{
			Hypothesis: "stricter prompt", BaselineRunRef: corev1alpha1.EvalObjectReference{Name: "baseline"},
		}},
	}
	plain := &corev1alpha1.EvalRun{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "demo", UID: "plain-uid"}}
	unreserved := candidate.DeepCopy()
	unreserved.Name, unreserved.UID = "unreserved", "unreserved-uid"
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(candidate, plain, unreserved).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	experimentStore := sqlite.NewStore(db, ":memory:")
	reservation, err := experimentStore.ReserveEvalExperiment(t.Context(), &store.EvalExperimentRecord{
		SchemaVersion: store.EvalExperimentSchemaVersion, Namespace: "demo", CandidateRun: "candidate", CandidateRunUID: "candidate-uid",
		BaselineRun: "baseline", BaselineRunUID: "baseline-uid", CaseSetDigest: "sha256:cases", CaseKeys: []string{"sha256:k"},
		GateDigest: "sha256:gate", Hypothesis: "stricter prompt", ReservedAt: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
	}, store.EvalExperimentLimits{MaxPerBaseline: 1, MaxPerCase: 1})
	require.NoError(t, err)
	require.True(t, reservation.Granted)
	env := &evalDecisionTestEnv{store: experimentStore, client: fakeClient, binding: evalDecisionTestBinding()}
	if result != "" {
		outcome := &store.EvalExperimentOutcome{
			SchemaVersion: store.EvalExperimentOutcomeSchemaVersion, Namespace: "demo", CandidateRun: "candidate",
			CandidateRunUID: "candidate-uid", Result: result, DecidedAt: time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC),
		}
		if result != store.EvalExperimentResultIncomparable {
			binding := env.binding
			outcome.Binding = &binding
		}
		_, err := experimentStore.RecordEvalExperimentOutcome(t.Context(), outcome)
		require.NoError(t, err)
	}
	binding := env.binding
	env.verifier = &fakeEvalDecisionVerifier{binding: &binding}
	// RBAC grants decide on evalruns in demo to everyone but carol.
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attrs := review.Spec.ResourceAttributes
		review.Status.Allowed = review.Spec.User != "carol" && attrs != nil && attrs.Verb == "decide" &&
			attrs.Group == corev1alpha1.GroupVersion.Group && attrs.Resource == "evalruns" && attrs.Namespace == "demo"
		return true, review, nil
	})
	handlers := NewHandlers(HandlersConfig{
		Client: fakeClient, KubeClient: clientset, EvalExperimentStore: experimentStore, EvalDecisionVerifier: env.verifier,
		EvalPromoterGroups: promoterGroups,
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		switch c.Get("X-Test-User") {
		case "":
		case "context-token":
			c.Locals(UserInfoContextKey, &UserInfo{Username: "alice", Groups: []string{"promoters"}, AuthType: AuthTypeContextToken, ContextToken: &ContextToken{}})
		default:
			c.Locals(UserInfoContextKey, &UserInfo{Username: c.Get("X-Test-User"), UID: "user-uid",
				Groups: strings.Split(c.Get("X-Test-Groups"), ","), AuthType: AuthTypeOIDC,
				Issuer: "https://issuer.example", Subject: "sub-" + c.Get("X-Test-User")})
		}
		return c.Next()
	})
	app.Get("/evals/runs/:name/decisions", handlers.GetEvalRunDecisions)
	app.Post("/evals/runs/:name/decisions", handlers.DecideEvalRun)
	app.Get("/evals/experiments", handlers.ListEvalExperiments)
	env.app = app
	return env
}

func (e *evalDecisionTestEnv) decide(t *testing.T, run, user, groups, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/evals/runs/"+run+"/decisions?namespace=demo", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", user)
	req.Header.Set("X-Test-Groups", groups)
	resp, err := e.app.Test(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(data)
}

func (e *evalDecisionTestEnv) get(t *testing.T, path string, out any) {
	t.Helper()
	resp, err := e.app.Test(httptest.NewRequest(http.MethodGet, path, nil))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
}

const approveBody = `{"decision":"approved","reason":"the evidence holds"}`

// Only a person in a promoter group may decide.
func TestDecideEvalRunAdmitsOnlyHumanPromoters(t *testing.T) {
	for name, test := range map[string]struct {
		groups     []string
		user, in   string
		wantStatus int
		wantError  string
	}{
		"decisions off":       {user: "alice", in: "promoters", wantStatus: http.StatusForbidden, wantError: "--eval-promoter-groups"},
		"anonymous":           {groups: []string{"promoters"}, wantStatus: http.StatusForbidden, wantError: "authenticated person"},
		"transaction token":   {groups: []string{"promoters"}, user: "context-token", wantStatus: http.StatusForbidden, wantError: "transaction tokens"},
		"service account":     {groups: []string{"promoters"}, user: "system:serviceaccount:demo:bot", in: "promoters", wantStatus: http.StatusForbidden, wantError: "not a ServiceAccount"},
		"system user":         {groups: []string{"promoters"}, user: "system:admin", in: "promoters", wantStatus: http.StatusForbidden, wantError: "not a ServiceAccount"},
		"no decide grant":     {groups: []string{"promoters"}, user: "carol", in: "promoters", wantStatus: http.StatusForbidden, wantError: "not authorized"},
		"outside every group": {groups: []string{"promoters"}, user: "alice", in: "developers", wantStatus: http.StatusForbidden, wantError: "promoter group"},
		"promoter":            {groups: []string{"promoters"}, user: "alice", in: "developers,promoters", wantStatus: http.StatusCreated},
	} {
		t.Run(name, func(t *testing.T) {
			env := setupEvalDecisionHandlers(t, store.EvalExperimentResultPassed, test.groups...)
			status, body := env.decide(t, "candidate", test.user, test.in, approveBody)
			require.Equal(t, test.wantStatus, status, body)
			require.Contains(t, body, test.wantError)
			if status == http.StatusCreated {
				var decision store.EvalDecisionRecord
				require.NoError(t, json.Unmarshal([]byte(body), &decision))
				require.Equal(t, store.EvalDecisionActor{Username: "alice", UID: "user-uid", Issuer: "https://issuer.example",
					Subject: "sub-alice", Groups: []string{"promoters"}}, decision.Actor)
				require.Equal(t, env.binding, *decision.Binding)
			}
		})
	}
}

// An approval needs a Passed outcome that still matches the live
// configuration; a rejection needs only an outcome.
func TestDecideEvalRunChecksTheExperiment(t *testing.T) {
	for name, test := range map[string]struct {
		result     string
		run, body  string
		mutate     func(*evalDecisionTestEnv)
		wantStatus int
		wantError  string
	}{
		"not an experiment":    {result: store.EvalExperimentResultPassed, run: "plain", body: approveBody, wantStatus: http.StatusConflict, wantError: "not an experiment"},
		"never started":        {result: store.EvalExperimentResultPassed, run: "unreserved", body: approveBody, wantStatus: http.StatusConflict, wantError: "never started"},
		"missing run":          {result: store.EvalExperimentResultPassed, run: "missing", body: approveBody, wantStatus: http.StatusNotFound},
		"undecided":            {run: "candidate", body: approveBody, wantStatus: http.StatusConflict, wantError: "not decided"},
		"approve a failure":    {result: store.EvalExperimentResultFailed, run: "candidate", body: approveBody, wantStatus: http.StatusConflict, wantError: "only a Passed"},
		"approve incomparable": {result: store.EvalExperimentResultIncomparable, run: "candidate", body: approveBody, wantStatus: http.StatusConflict, wantError: "only a Passed"},
		"candidate changed": {result: store.EvalExperimentResultPassed, run: "candidate", body: approveBody,
			mutate:     func(e *evalDecisionTestEnv) { e.verifier.binding.CandidateAgent.Generation = 2 },
			wantStatus: http.StatusConflict, wantError: `candidate Agent "candidate-agent" was edited`},
		"cases and graders changed": {result: store.EvalExperimentResultPassed, run: "candidate", body: approveBody,
			mutate: func(e *evalDecisionTestEnv) {
				e.verifier.binding.CaseSetDigest, e.verifier.binding.GraderSetDigest = "sha256:new", "sha256:new"
			},
			wantStatus: http.StatusConflict, wantError: "the case set changed; the grader set changed"},
		"unverifiable": {result: store.EvalExperimentResultPassed, run: "candidate", body: approveBody,
			mutate: func(e *evalDecisionTestEnv) {
				e.verifier.err = errors.New("reviewer agent \"candidate-agent\" was not found")
			},
			wantStatus: http.StatusConflict, wantError: "cannot be checked"},
		"reject a failure": {result: store.EvalExperimentResultFailed, run: "candidate", body: `{"decision":"rejected","reason":"regressed"}`, wantStatus: http.StatusCreated},
		"reject a change": {result: store.EvalExperimentResultPassed, run: "candidate", body: `{"decision":"rejected","reason":"not now"}`,
			mutate: func(e *evalDecisionTestEnv) { e.verifier.binding.GateDigest = "sha256:new" }, wantStatus: http.StatusCreated},
		"bad decision": {result: store.EvalExperimentResultPassed, run: "candidate", body: `{"decision":"maybe","reason":"x"}`, wantStatus: http.StatusBadRequest},
		"no reason":    {result: store.EvalExperimentResultPassed, run: "candidate", body: `{"decision":"approved","reason":"  "}`, wantStatus: http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			env := setupEvalDecisionHandlers(t, test.result, "promoters")
			if test.mutate != nil {
				test.mutate(env)
			}
			status, body := env.decide(t, test.run, "alice", "promoters", test.body)
			require.Equal(t, test.wantStatus, status, body)
			require.Contains(t, body, test.wantError)
		})
	}
}

// Decisions only append, the latest is in force, and a later change to the
// live configuration makes an effective approval stale. Experiments stay
// listed after their runs are deleted.
func TestEvalDecisionsAppendAndStayQueryable(t *testing.T) {
	env := setupEvalDecisionHandlers(t, store.EvalExperimentResultPassed, "promoters")
	status, body := env.decide(t, "candidate", "alice", "promoters", approveBody)
	require.Equal(t, http.StatusCreated, status, body)

	var response EvalExperimentResponse
	env.get(t, "/evals/runs/candidate/decisions?namespace=demo", &response)
	require.Len(t, response.Decisions, 1)
	require.Equal(t, store.EvalDecisionApproved, response.Effective.Decision)
	require.Equal(t, &EvalApprovalStatus{Current: true}, response.Approval)

	env.verifier.binding.CandidateDigest = "sha256:edited"
	env.get(t, "/evals/runs/candidate/decisions?namespace=demo", &response)
	require.False(t, response.Approval.Current)
	require.Equal(t, []string{"the candidate's prompt, model or runtime image changed"}, response.Approval.Changed)

	status, body = env.decide(t, "candidate", "bob", "promoters", `{"decision":"rejected","reason":"the prompt changed"}`)
	require.Equal(t, http.StatusCreated, status, body)
	response = EvalExperimentResponse{}
	env.get(t, "/evals/runs/candidate/decisions?namespace=demo", &response)
	require.Len(t, response.Decisions, 2)
	require.Equal(t, store.EvalDecisionRejected, response.Effective.Decision)
	require.Equal(t, "bob", response.Effective.Actor.Username)
	require.Nil(t, response.Approval)

	run := &corev1alpha1.EvalRun{}
	require.NoError(t, env.client.Get(t.Context(), client.ObjectKey{Namespace: "demo", Name: "candidate"}, run))
	require.NoError(t, env.client.Delete(t.Context(), run))
	var list struct {
		Items    []EvalExperimentResponse `json:"items"`
		Continue string                   `json:"continue"`
	}
	env.get(t, "/evals/experiments?namespace=demo", &list)
	require.Len(t, list.Items, 1)
	require.Equal(t, "candidate", list.Items[0].Experiment.CandidateRun)
	require.Equal(t, store.EvalExperimentResultPassed, list.Items[0].Outcome.Result)
	require.Len(t, list.Items[0].Decisions, 2)
	require.Equal(t, store.EvalDecisionRejected, list.Items[0].Effective.Decision)
	require.Empty(t, list.Continue)

	resp, err := env.app.Test(httptest.NewRequest(http.MethodGet, "/evals/experiments?namespace=demo&limit=0", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp, err = env.app.Test(httptest.NewRequest(http.MethodGet, "/evals/experiments?namespace=demo&continue=bogus", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
