package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func setupEvalTrialHandlers(t *testing.T) (*fiber.App, *sqlite.Store) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	run := &corev1alpha1.EvalRun{ObjectMeta: metav1.ObjectMeta{Name: "pr-review", Namespace: "demo", UID: "run-uid"}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	trialStore := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{Client: fakeClient, EvalTrialStore: trialStore})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if c.Get("X-Test-Context-Token") != "" {
			c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeContextToken, ContextToken: &ContextToken{}})
		}
		return c.Next()
	})
	app.Get("/evals/runs/:name/trials", handlers.ListEvalRunTrials)
	return app, trialStore
}

func evalTrialHandlerRecord(uid, caseID string, trial int32) *store.EvalTrialRecord {
	return &store.EvalTrialRecord{
		SchemaVersion: store.EvalTrialSchemaVersion,
		Namespace:     "demo",
		EvalRun:       "pr-review",
		EvalRunUID:    uid,
		CaseID:        caseID,
		Trial:         trial,
		TaskName:      "eval-" + caseID,
		Outcome:       store.EvalTrialOutcomePass,
		CompletedAt:   time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
	}
}

func TestListEvalRunTrials(t *testing.T) {
	app, trialStore := setupEvalTrialHandlers(t)
	require.NoError(t, trialStore.CreateEvalTrial(t.Context(), evalTrialHandlerRecord("run-uid", "a", 1)))
	require.NoError(t, trialStore.CreateEvalTrial(t.Context(), evalTrialHandlerRecord("run-uid", "b", 1)))
	// A record of an earlier incarnation with the same name is not served.
	require.NoError(t, trialStore.CreateEvalTrial(t.Context(), evalTrialHandlerRecord("old-uid", "c", 1)))

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/evals/runs/pr-review/trials?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		Items []store.EvalTrialRecord `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Items, 2)
	require.Equal(t, "a", body.Items[0].CaseID)

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/evals/runs/pr-review/trials?namespace=demo&format=jsonl", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, evalTrialsJSONLContentType, resp.Header.Get(fiber.HeaderContentType))
	scanner := bufio.NewScanner(resp.Body)
	var lines []store.EvalTrialRecord
	for scanner.Scan() {
		var record store.EvalTrialRecord
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &record))
		lines = append(lines, record)
	}
	require.Len(t, lines, 2)
	require.Equal(t, "b", lines[1].CaseID)

	req := httptest.NewRequest(http.MethodGet, "/evals/runs/pr-review/trials?namespace=demo", nil)
	req.Header.Set(fiber.HeaderAccept, "application/jsonl")
	resp, err = app.Test(req)
	require.NoError(t, err)
	require.Equal(t, evalTrialsJSONLContentType, resp.Header.Get(fiber.HeaderContentType))
}

func TestListEvalRunTrialsErrors(t *testing.T) {
	app, _ := setupEvalTrialHandlers(t)
	for name, tc := range map[string]struct {
		path   string
		token  bool
		status int
	}{
		"missing run":       {path: "/evals/runs/missing/trials?namespace=demo", status: http.StatusNotFound},
		"bad format":        {path: "/evals/runs/pr-review/trials?namespace=demo&format=csv", status: http.StatusBadRequest},
		"transaction token": {path: "/evals/runs/pr-review/trials?namespace=demo", token: true, status: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.token {
				req.Header.Set("X-Test-Context-Token", "1")
			}
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, tc.status, resp.StatusCode)
		})
	}

	empty, _ := setupEvalTrialHandlers(t)
	resp, err := empty.Test(httptest.NewRequest(http.MethodGet, "/evals/runs/pr-review/trials?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.JSONEq(t, `{"items":[]}`, readRespBody(t, resp))
}
