/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/orka-agents/orka/internal/store/sqlite"
	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
)

type oversightTestEnv struct {
	h       *Handlers
	app     *fiber.App
	reviews *[]authorizationv1.ResourceAttributes
}

// newOversightTestEnv serves the oversight routes over a real SQLite store.
// Callers name themselves with the X-Test-User header; RBAC allows exactly
// the "user|verb|resource" grants given.
func newOversightTestEnv(t *testing.T, enabled bool, grants ...string) *oversightTestEnv {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	backend := sqlite.NewStore(db, ":memory:")
	t.Cleanup(func() { _ = backend.Close() })
	allowed := map[string]bool{}
	for _, grant := range grants {
		allowed[grant] = true
	}
	reviews := &[]authorizationv1.ResourceAttributes{}
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attributes := review.Spec.ResourceAttributes
		*reviews = append(*reviews, *attributes)
		resource := attributes.Resource
		if attributes.Subresource != "" {
			resource += "/" + attributes.Subresource
		}
		review.Status.Allowed = allowed[review.Spec.User+"|"+attributes.Verb+"|"+resource]
		return true, review, nil
	})
	h := NewHandlers(HandlersConfig{
		KubeClient: clientset, OversightStore: testStorageClient(backend), OversightEnabled: enabled,
	})
	h.eventStreamPollInterval, h.eventStreamHeartbeatEvery = 5*time.Millisecond, 5*time.Millisecond
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if user := c.Get("X-Test-User"); user != "" {
			info := &UserInfo{Username: user, AuthType: AuthTypeTokenReview}
			info.Namespace = parseServiceAccountNamespace(user)
			c.Locals(UserInfoContextKey, info)
		}
		return c.Next()
	})
	app.Post("/internal/v1/oversight/:namespace/decisions", h.IngestOversightDecisions)
	app.Get("/api/v1/oversight/posture", h.GetOversightPosture)
	app.Get("/api/v1/oversight/config", h.GetOversightConfig)
	app.Get("/api/v1/oversight/decisions", h.ListOversightDecisions)
	app.Get("/api/v1/oversight/decisions/stream", h.StreamOversightDecisions)
	app.Get("/api/v1/oversight/decisions/groups", h.GroupOversightDecisions)
	app.Post("/api/v1/oversight/decisions/bulk-feedback", h.BulkOversightFeedback)
	app.Get("/api/v1/oversight/decisions/:id", h.GetOversightDecision)
	app.Get("/api/v1/oversight/decisions/:id/similar", h.SimilarOversightDecisions)
	app.Post("/api/v1/oversight/decisions/:id/feedback", h.RecordOversightFeedback)
	return &oversightTestEnv{h: h, app: app, reviews: reviews}
}

const (
	ingestSA = "system:serviceaccount:team:oversight-ingest"
	viewer   = "alice"
)

var viewerGrants = []string{viewer + "|list|oversightdecisions", viewer + "|get|oversightdecisions", viewer + "|create|oversightdecisions/feedback"}

func (e *oversightTestEnv) do(t *testing.T, method, target, user string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, ok := body.([]byte)
		if !ok {
			var err error
			raw, err = json.Marshal(body)
			require.NoError(t, err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, data
}

func testOversightDecision(id, task, agent, dest string, observed time.Time) oversightv1.OversightDecision {
	d := oversightv1.OversightDecision{
		ID: id, TaskName: task, AgentName: agent, ObservedAt: observed,
		Mode: oversightv1.ModeAudit, Disposition: oversightv1.DispositionAllow,
		EvidenceLevel: oversightv1.EvidenceLevelObserved,
		Coverage:      oversightv1.ActionCoverage{StreamState: "ok", SensorState: "events"},
		Action: oversightv1.ActionEnvelope{
			ToolName: "code_exec", RequestSummary: "fetch a model",
			Effects: oversightv1.ObservedEffects{
				Counts:   map[string]int{"exec": 1, "net_connect": 1},
				Binaries: []oversightv1.CountedValue{{Value: "/usr/bin/python3", Count: 1}},
			},
		},
		Scorers: []oversightv1.ScorerEvaluation{{ProviderRef: "scorer-a", ModelID: "model-a", Status: "ok",
			Scores: []oversightv1.ConcernScore{{QuestionID: "K2", Probability: 0.1}}}},
	}
	if dest != "" {
		d.Action.Effects.Destinations = []oversightv1.CountedValue{{Value: dest, Count: 1}}
	}
	return d
}

func (e *oversightTestEnv) ingest(t *testing.T, decisions ...oversightv1.OversightDecision) oversightv1.IngestResponse {
	t.Helper()
	status, body := e.do(t, http.MethodPost, "/internal/v1/oversight/team/decisions", ingestSA, oversightv1.IngestRequest{Decisions: decisions})
	require.Equal(t, http.StatusOK, status, string(body))
	var response oversightv1.IngestResponse
	require.NoError(t, json.Unmarshal(body, &response))
	return response
}

func TestOversightRoutesAnswerNotImplementedWhenDisabled(t *testing.T) {
	e := newOversightTestEnv(t, false, ingestSA+"|create|oversightdecisions")
	status, _ := e.do(t, http.MethodGet, "/api/v1/oversight/posture?namespace=team", viewer, nil)
	require.Equal(t, http.StatusNotImplemented, status)
	status, _ = e.do(t, http.MethodPost, "/internal/v1/oversight/team/decisions", ingestSA, oversightv1.IngestRequest{})
	require.Equal(t, http.StatusNotImplemented, status)
	status, _ = e.do(t, http.MethodPost, "/api/v1/oversight/decisions/x/feedback?namespace=team", viewer, oversightv1.FeedbackRequest{Label: "unclear"})
	require.Equal(t, http.StatusNotImplemented, status)
}

func TestOversightIngestNeedsAnAuthorizedNonWorkerIdentity(t *testing.T) {
	worker := "system:serviceaccount:team:orka-ai-worker"
	e := newOversightTestEnv(t, true, ingestSA+"|create|oversightdecisions", worker+"|create|oversightdecisions")
	now := time.Now().UTC()
	batch := oversightv1.IngestRequest{Decisions: []oversightv1.OversightDecision{testOversightDecision("a@0", "a", "agent-a", "", now)}}

	for name, tc := range map[string]struct {
		user   string
		path   string
		status int
	}{
		"anonymous":                {"", "/internal/v1/oversight/team/decisions", http.StatusUnauthorized},
		"a worker, even with RBAC": {worker, "/internal/v1/oversight/team/decisions", http.StatusForbidden},
		"another namespace":        {"system:serviceaccount:other:oversight-ingest", "/internal/v1/oversight/team/decisions", http.StatusForbidden},
		"without RBAC":             {"system:serviceaccount:team:someone", "/internal/v1/oversight/team/decisions", http.StatusForbidden},
		"a person":                 {viewer, "/internal/v1/oversight/team/decisions", http.StatusForbidden},
		"the ingest identity":      {ingestSA, "/internal/v1/oversight/team/decisions", http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := e.do(t, http.MethodPost, tc.path, tc.user, batch)
			require.Equal(t, tc.status, status, string(body))
		})
	}
	require.Contains(t, *e.reviews, authorizationv1.ResourceAttributes{
		Namespace: "team", Verb: "create", Group: "core.orka.ai", Resource: "oversightdecisions",
	})
}

func TestOversightIngestIsIdempotentRedactedAndBounded(t *testing.T) {
	e := newOversightTestEnv(t, true, append(viewerGrants, ingestSA+"|create|oversightdecisions")...)
	now := time.Now().UTC().Truncate(time.Second)
	d := testOversightDecision("a@0", "a", "agent-a", "modelhub.test:443", now)
	d.Action.RequestSummary = "curl -H 'Authorization: Bearer sekret-token-value' https://modelhub.test/x?token=abc"
	d.Seq = 77

	first := e.ingest(t, d)
	require.Equal(t, 1, first.Ingested)
	again := e.ingest(t, d)
	require.Equal(t, 1, again.Duplicates)
	require.Equal(t, first.Results[0].Seq, again.Results[0].Seq)

	changed := d
	changed.Disposition = oversightv1.DispositionAuditFlagged
	status, _ := e.do(t, http.MethodPost, "/internal/v1/oversight/team/decisions", ingestSA, oversightv1.IngestRequest{Decisions: []oversightv1.OversightDecision{changed}})
	require.Equal(t, http.StatusConflict, status)

	status, body := e.do(t, http.MethodGet, "/api/v1/oversight/decisions/a@0?namespace=team", viewer, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NotContains(t, string(body), "sekret-token-value")
	require.NotContains(t, string(body), "token=abc")
	var view oversightv1.DecisionView
	require.NoError(t, json.Unmarshal(body, &view))
	require.Equal(t, first.Results[0].Seq, view.Seq, "the store assigns Seq")
	require.Equal(t, "team", view.Namespace)
	require.Contains(t, view.ActionNarrative, "connected to modelhub.test:443")

	status, _ = e.do(t, http.MethodPost, "/internal/v1/oversight/team/decisions", ingestSA, bytes.Repeat([]byte("x"), maxOversightIngestBytes+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, status)
	status, _ = e.do(t, http.MethodPost, "/internal/v1/oversight/team/decisions", ingestSA, []byte(`{"decisions":[],"extra":1}`))
	require.Equal(t, http.StatusBadRequest, status)
	bad := testOversightDecision("b@0", "b", "", "", now)
	bad.Disposition = "approved"
	status, body = e.do(t, http.MethodPost, "/internal/v1/oversight/team/decisions", ingestSA, oversightv1.IngestRequest{Decisions: []oversightv1.OversightDecision{bad}})
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, string(body), "decisions[0]")
}

func TestOversightReadsAreNamespaceAuthorized(t *testing.T) {
	e := newOversightTestEnv(t, true, viewer+"|list|oversightdecisions")
	status, _ := e.do(t, http.MethodGet, "/api/v1/oversight/decisions?namespace=team", "mallory", nil)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = e.do(t, http.MethodGet, "/api/v1/oversight/decisions?namespace=team", viewer, nil)
	require.Equal(t, http.StatusOK, status)
	status, _ = e.do(t, http.MethodGet, "/api/v1/oversight/decisions/x?namespace=team", viewer, nil)
	require.Equal(t, http.StatusForbidden, status, "detail needs get")
}

func listDecisions(t *testing.T, e *oversightTestEnv, query string) oversightv1.DecisionListResponse {
	t.Helper()
	status, body := e.do(t, http.MethodGet, "/api/v1/oversight/decisions?namespace=team&"+query, viewer, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var response oversightv1.DecisionListResponse
	require.NoError(t, json.Unmarshal(body, &response))
	return response
}

func ids(views []oversightv1.DecisionView) []string {
	out := make([]string, 0, len(views))
	for _, view := range views {
		out = append(out, view.ID)
	}
	return out
}

func TestOversightSearchSimilarGroupsAndFeedback(t *testing.T) {
	e := newOversightTestEnv(t, true, append(viewerGrants, ingestSA+"|create|oversightdecisions")...)
	base := time.Now().UTC().Add(-48 * time.Hour)
	e.ingest(t,
		testOversightDecision("a@0", "task-a", "analyst", "modelhub.test:443", base),
		testOversightDecision("b@0", "task-b", "analyst", "cdn-lfs.modelhub.test:443", base.Add(time.Hour)),
		testOversightDecision("c@0", "task-c", "builder", "modelhub.test:443", base.Add(2*time.Hour)),
		testOversightDecision("d@0", "task-d", "builder", "pypi.org:443", base.Add(3*time.Hour)),
	)

	response := listDecisions(t, e, "q=domain:modelhub.test")
	require.Equal(t, []string{"c@0", "b@0", "a@0"}, ids(response.Items), "newest first")
	require.Equal(t, []oversightv1.QueryTerm{{Key: "domain", Value: "modelhub.test"}}, response.Query)
	require.Equal(t, []string{"a@0"}, ids(listDecisions(t, e, "q=dest:modelhub.test:443+agent:analyst").Items))
	require.Equal(t, []string{"d@0"}, ids(listDecisions(t, e, "q=pypi").Items), "free text")
	require.Empty(t, listDecisions(t, e, "q=domain:modelhub.test&window=24h").Items)
	page := listDecisions(t, e, "limit=2")
	require.Equal(t, []string{"d@0", "c@0"}, ids(page.Items))
	require.NotEmpty(t, page.Continue)
	require.Equal(t, []string{"b@0", "a@0"}, ids(listDecisions(t, e, "limit=2&continue="+page.Continue).Items))
	status, _ := e.do(t, http.MethodGet, "/api/v1/oversight/decisions?namespace=team&q=disposition:bogus", viewer, nil)
	require.Equal(t, http.StatusBadRequest, status)

	status, body := e.do(t, http.MethodGet, "/api/v1/oversight/decisions/a@0/similar?namespace=team", viewer, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var similar oversightv1.SimilarResponse
	require.NoError(t, json.Unmarshal(body, &similar))
	require.Equal(t, "c@0", similar.Items[0].Decision.ID)
	require.True(t, similar.Items[0].ExactSignature)
	require.Equal(t, "b@0", similar.Items[1].Decision.ID)
	require.NotEmpty(t, similar.Items[1].Shared)

	status, body = e.do(t, http.MethodGet, "/api/v1/oversight/decisions/groups?namespace=team&by=agent&q=domain:modelhub.test", viewer, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var groups oversightv1.GroupsResponse
	require.NoError(t, json.Unmarshal(body, &groups))
	require.Equal(t, "analyst", groups.Groups[0].Key)
	require.Equal(t, 2, groups.Groups[0].Count)
	require.True(t, groups.Groups[0].FirstSeen.Before(groups.Groups[0].LastSeen))

	status, body = e.do(t, http.MethodPost, "/api/v1/oversight/decisions/bulk-feedback?namespace=team", viewer,
		oversightv1.BulkFeedbackRequest{IDs: []string{"a@0", "b@0"}, Label: oversightv1.FeedbackFalseAlarm, Note: "expected model download"})
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), `"recorded":2`)
	require.Equal(t, []string{"b@0", "a@0"}, ids(listDecisions(t, e, "q=feedback:false_alarm").Items))
	require.Equal(t, oversightv1.FeedbackFalseAlarm, listDecisions(t, e, "q=task:task-a").Items[0].Feedback.Label)
	require.Equal(t, viewer, listDecisions(t, e, "q=task:task-a").Items[0].Feedback.Reviewer)

	status, _ = e.do(t, http.MethodPost, "/api/v1/oversight/decisions/a@0/feedback?namespace=team", viewer, oversightv1.FeedbackRequest{Label: "approve"})
	require.Equal(t, http.StatusBadRequest, status, "labels are calibration labels, never decisions")
	status, _ = e.do(t, http.MethodPost, "/api/v1/oversight/decisions/missing/feedback?namespace=team", viewer, oversightv1.FeedbackRequest{Label: "unclear"})
	require.Equal(t, http.StatusNotFound, status)
	status, _ = e.do(t, http.MethodPost, "/api/v1/oversight/decisions/a@0/feedback?namespace=team",
		"system:serviceaccount:team:orka-ai-worker", oversightv1.FeedbackRequest{Label: "false_alarm"})
	require.Equal(t, http.StatusForbidden, status, "an agent cannot label its own findings")
}

func TestOversightPostureInAuditModeIsNeverWarranted(t *testing.T) {
	e := newOversightTestEnv(t, true, append(viewerGrants, ingestSA+"|create|oversightdecisions")...)
	status, body := e.do(t, http.MethodGet, "/api/v1/oversight/posture?namespace=team", viewer, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var posture oversightv1.PostureResponse
	require.NoError(t, json.Unmarshal(body, &posture))
	require.Equal(t, oversightv1.ConfidenceBlind, posture.Confidence.State)

	now := time.Now().UTC()
	against := testOversightDecision("b@0", "b", "pusher", "registry.example:443", now)
	against.CharterFindings = []oversightv1.CharterFinding{{ProducerRef: "rules", BoundID: "B2", Sentence: "Never push images to the registry."}}
	e.ingest(t, testOversightDecision("a@0", "a", "analyst", "", now))
	status, body = e.do(t, http.MethodGet, "/api/v1/oversight/posture?namespace=team", viewer, nil)
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, json.Unmarshal(body, &posture))
	require.Equal(t, oversightv1.ConfidenceDegraded, posture.Confidence.State)
	require.Equal(t, oversightv1.VisibilityFull, posture.Visibility.State)
	require.Equal(t, oversightv1.ControlNotEnforced, posture.Control.State)

	e.ingest(t, against)
	_, body = e.do(t, http.MethodGet, "/api/v1/oversight/posture?namespace=team", viewer, nil)
	require.NoError(t, json.Unmarshal(body, &posture))
	require.Equal(t, oversightv1.ConfidenceAttentionRequired, posture.Confidence.State)
	require.Equal(t, 1, posture.Basis.CharterFindings)
}

func TestOversightStreamReplaysAfterTheCursorAndFollows(t *testing.T) {
	e := newOversightTestEnv(t, true, append(viewerGrants, ingestSA+"|create|oversightdecisions")...)
	now := time.Now().UTC()
	first := e.ingest(t, testOversightDecision("a@0", "a", "agent", "", now), testOversightDecision("a@1", "a", "agent", "", now))
	go func() {
		time.Sleep(30 * time.Millisecond)
		e.ingest(t, testOversightDecision("a@2", "a", "agent", "", now))
	}()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/oversight/decisions/stream?namespace=team&after=%d", first.Results[0].Seq), nil)
	req.Header.Set("X-Test-User", viewer)
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 300 * time.Millisecond})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	data, _ := io.ReadAll(resp.Body)
	stream := string(data)
	require.Contains(t, stream, "event: ready")
	require.NotContains(t, stream, `"id":"a@0"`, "the cursor is exclusive")
	require.Contains(t, stream, `"id":"a@1"`)
	require.Contains(t, stream, `"id":"a@2"`, "new decisions stream without reconnecting")
	require.Contains(t, stream, ": heartbeat")
	require.Less(t, strings.Index(stream, `"id":"a@1"`), strings.Index(stream, `"id":"a@2"`))
}
