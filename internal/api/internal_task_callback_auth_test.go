/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/workerenv"
)

// testJWT builds an unsigned token with the given aud claim; TokenReview
// fakes decide whether it authenticates.
func testJWT(t *testing.T, audience string) string {
	t.Helper()
	encode := base64.RawURLEncoding.EncodeToString
	payload := fmt.Sprintf(`{"aud":%s,"sub":"system:serviceaccount:default:worker","nbf":%d}`, audience, time.Now().UnixNano())
	return encode([]byte(`{"alg":"RS256"}`)) + "." + encode([]byte(payload)) + "." + encode([]byte("signature"))
}

func TestJWTAudienceIncludes(t *testing.T) {
	const audience = workerenv.TaskCallbackTokenAudience
	for _, tt := range []struct {
		name  string
		token string
		want  bool
	}{
		{name: "string claim", token: testJWT(t, `"`+audience+`"`), want: true},
		{name: "list claim", token: testJWT(t, `["https://kubernetes.default.svc","`+audience+`"]`), want: true},
		{name: "other audience", token: testJWT(t, `["https://kubernetes.default.svc"]`)},
		{name: "prefix only", token: testJWT(t, `"`+audience+`-extra"`)},
		{name: "missing claim", token: testJWT(t, `null`)},
		{name: "opaque token", token: "not-a-jwt"},
		{name: "undecodable payload", token: "a.!!!.c"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, jwtAudienceIncludes(tt.token, audience))
		})
	}
}

type taskCallbackFixture struct {
	server   *Server
	reviews  *[][]string
	callback string
	defaultT string
}

// newTaskCallbackFixture serves the real routes. The callback token
// authenticates only when reviewed against the callback audience, as the
// Kubernetes API server does for a projected token with that audience; the
// default token authenticates only against the default audiences.
func newTaskCallbackFixture(t *testing.T) taskCallbackFixture {
	t.Helper()
	scheme := internalCallerAuthScheme(t)
	require.NoError(t, authenticationv1.AddToScheme(scheme))
	task := internalCallerAuthTask()
	job := internalCallerAuthJob(task, "job-a", "job-uid")
	pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
	worker := internalCallerAuthWorkerUser("pod-a", "pod-uid")
	f := taskCallbackFixture{
		reviews:  &[][]string{},
		callback: testJWT(t, `["`+workerenv.TaskCallbackTokenAudience+`"]`),
		defaultT: testJWT(t, `["https://kubernetes.default.svc"]`),
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(task).WithObjects(task, job, pod).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			review, ok := obj.(*authenticationv1.TokenReview)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			*f.reviews = append(*f.reviews, slices.Clone(review.Spec.Audiences))
			callbackReview := slices.Equal(review.Spec.Audiences, []string{workerenv.TaskCallbackTokenAudience})
			if (review.Spec.Token == f.callback && callbackReview) || (review.Spec.Token == f.defaultT && len(review.Spec.Audiences) == 0) {
				review.Status = authenticationv1.TokenReviewStatus{
					Authenticated: true,
					Audiences:     review.Spec.Audiences,
					User:          authenticationv1.UserInfo{Username: worker.Username, UID: "worker-sa-uid", Extra: worker.Extra},
				}
			}
			return nil
		}}).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	results := sqlite.NewStore(db, ":memory:")
	f.server = NewServer(kube, nil, ServerConfig{
		ResultStore: results, ArtifactStore: results, PlanStore: results, ExecutionEventStore: results, APIReader: kube,
	})
	return f
}

func (f taskCallbackFixture) request(t *testing.T, method, path, token string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.server.app.Test(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestTaskCallbackTokenPostsOnlyResultsArtifactsAndEvents(t *testing.T) {
	tokenCache.Clear()
	f := newTaskCallbackFixture(t)

	require.Equal(t, http.StatusNoContent,
		f.request(t, http.MethodPost, "/internal/v1/results/default/task-a", f.callback, []byte(`{"version":1,"summary":"ok"}`)))
	require.Equal(t, http.StatusCreated,
		f.request(t, http.MethodPost, "/internal/v1/artifacts/default/task-a/shot.png", f.callback, []byte("png")))
	require.Equal(t, http.StatusCreated,
		f.request(t, http.MethodPost, "/internal/v1/events/default/task/task-a", f.callback, []byte(`{"type":"WorkerStarted"}`)))
	for _, audiences := range *f.reviews {
		require.Equal(t, []string{workerenv.TaskCallbackTokenAudience}, audiences,
			"a callback token must be reviewed against its own audience only")
	}

	// Every other internal route reviews against the default audiences,
	// which the callback token cannot satisfy.
	require.Equal(t, http.StatusUnauthorized,
		f.request(t, http.MethodPost, "/internal/v1/plans/default/task-a", f.callback, []byte(`{"summary":"plan"}`)))
	require.Equal(t, http.StatusUnauthorized,
		f.request(t, http.MethodGet, "/internal/v1/plans/default/task-a", f.callback, nil))
	// A path that is not a single artifact name falls through to those routes.
	require.Equal(t, http.StatusUnauthorized,
		f.request(t, http.MethodPost, "/internal/v1/artifacts/default/task-a/nested/shot.png", f.callback, []byte("png")))
}

func TestTaskCallbackRoutesStillAcceptDefaultWorkerTokens(t *testing.T) {
	tokenCache.Clear()
	f := newTaskCallbackFixture(t)

	require.Equal(t, http.StatusNoContent,
		f.request(t, http.MethodPost, "/internal/v1/results/default/task-a", f.defaultT, []byte(`{"version":1,"summary":"ok"}`)))
	require.Len(t, *f.reviews, 1, "a default worker token must be reviewed once")
	require.Empty(t, (*f.reviews)[0], "a default worker token must be reviewed against the default audiences")
}
