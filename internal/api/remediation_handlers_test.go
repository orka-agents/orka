package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/workers/common"
)

type remediationServiceCall struct {
	operation, namespace, id, subject string
	request                           contract.Request
}

type remediationServiceStub struct {
	calls  []remediationServiceCall
	status *contract.Status
	patch  []byte
	err    error
}

func newRemediationServiceStub() *remediationServiceStub {
	return &remediationServiceStub{
		status: &contract.Status{
			ID: "request-current", Namespace: "default", Phase: contract.PhasePending,
			Stage: contract.StageReproduce, Reason: "accepted",
		},
		patch: []byte("diff --git a/sample.txt b/sample.txt\n--- a/sample.txt\n+++ b/sample.txt\n@@ -1 +1 @@\n-old\n+new\n"),
	}
}

func (s *remediationServiceStub) Submit(_ context.Context, namespace, subject string, request contract.Request) (*contract.Status, error) {
	s.calls = append(s.calls, remediationServiceCall{operation: "submit", namespace: namespace, id: request.RequestID, subject: subject, request: request})
	return s.status, s.err
}

func (s *remediationServiceStub) Get(_ context.Context, namespace, id string) (*contract.Status, error) {
	s.calls = append(s.calls, remediationServiceCall{operation: "get", namespace: namespace, id: id})
	return s.status, s.err
}

func (s *remediationServiceStub) Cancel(_ context.Context, namespace, id string) (*contract.Status, error) {
	s.calls = append(s.calls, remediationServiceCall{operation: "cancel", namespace: namespace, id: id})
	return s.status, s.err
}

func (s *remediationServiceStub) Patch(_ context.Context, namespace, id string) ([]byte, error) {
	s.calls = append(s.calls, remediationServiceCall{operation: "patch", namespace: namespace, id: id})
	return s.patch, s.err
}

func remediationTestRequest() contract.Request {
	return contract.Request{
		RequestID: "request-current", Mode: contract.Generate,
		Report:     json.RawMessage(`{"title":"Synthetic quantity check","problem":"A negative quantity is accepted"}`),
		Repository: "https://github.com/example/synthetic", Ref: "v1.0.0",
	}
}

func remediationRequestJSON(t *testing.T, request contract.Request) string {
	t.Helper()
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	return string(raw)
}

func remediationTestUser() *UserInfo {
	return &UserInfo{
		AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:caller",
		UID: "caller-uid", Namespace: "default", Groups: []string{"system:authenticated"},
		Extra: map[string]authenticationv1.ExtraValue{"example.test/claim": {"fixture"}},
	}
}

func remediationHandlerApp(h *Handlers, user *UserInfo, middleware ...fiber.Handler) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: customErrorHandler, BodyLimit: maxRemediationRequestBytes + 1, StreamRequestBody: true,
	})
	app.Use(remediationPrivateResponse)
	app.Use(func(c fiber.Ctx) error {
		if user != nil {
			c.Locals(UserInfoContextKey, user)
		}
		return c.Next()
	})
	app.Use(NewLoggingMiddleware())
	for _, handler := range middleware {
		app.Use(handler)
	}
	app.Post("/api/v1/remediations", h.CreateRemediation)
	app.Get("/api/v1/remediations/:id", h.GetRemediation)
	app.Post("/api/v1/remediations/:id/cancel", h.CancelRemediation)
	app.Get("/api/v1/remediations/:id/patch", h.GetRemediationPatch)
	return app
}

func remediationAllowClient(reviews *[]authorizationv1.SubjectAccessReviewSpec) *kubefake.Clientset {
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		*reviews = append(*reviews, *review.Spec.DeepCopy())
		review.Status.Allowed = true
		return true, review, nil
	})
	return clientset
}

func remediationHTTPCall(t *testing.T, app *fiber.App, method, path, body string) (int, string, http.Header) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", fmt.Sprintf("Bearer synthetic-fixture-%s-%d", t.Name(), time.Now().UnixNano()))
	response, err := app.Test(request, fiber.TestConfig{Timeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(content), response.Header
}

func remediationRouteCases(t *testing.T) []struct {
	method, path, body, operation string
	permission                    authorizationv1.ResourceAttributes
	status                        int
} {
	t.Helper()
	return []struct {
		method, path, body, operation string
		permission                    authorizationv1.ResourceAttributes
		status                        int
	}{
		{http.MethodPost, "/api/v1/remediations", remediationRequestJSON(t, remediationTestRequest()), "submit",
			authorizationv1.ResourceAttributes{Namespace: "default", Group: "core.orka.ai", Resource: "remediations", Verb: "create"}, http.StatusAccepted},
		{http.MethodGet, "/api/v1/remediations/request-current", "", "get",
			authorizationv1.ResourceAttributes{Namespace: "default", Group: "core.orka.ai", Resource: "remediations", Verb: "get", Name: "request-current"}, http.StatusOK},
		{http.MethodPost, "/api/v1/remediations/request-current/cancel", "", "cancel",
			authorizationv1.ResourceAttributes{Namespace: "default", Group: "core.orka.ai", Resource: "remediations", Subresource: "cancel", Verb: "update", Name: "request-current"}, http.StatusAccepted},
		{http.MethodGet, "/api/v1/remediations/request-current/patch", "", "patch",
			authorizationv1.ResourceAttributes{Namespace: "default", Group: "core.orka.ai", Resource: "remediations", Subresource: "patch", Verb: "get", Name: "request-current"}, http.StatusOK},
	}
}

func TestExternalAPIRemediationAuthorization(t *testing.T) {
	for _, route := range remediationRouteCases(t) {
		t.Run(route.operation, func(t *testing.T) {
			for _, decision := range []string{"allowed", "denied", "review-error", "evaluation-error", "explicit-denied", "missing-client", "typed-nil-client"} {
				t.Run(decision, func(t *testing.T) {
					f := newExternalAuthorizationFixture(t)
					service := newRemediationServiceStub()
					f.server.handlers.remediationService = service
					if decision == "missing-client" {
						f.server.handlers.clientset = nil
					}
					if decision == "typed-nil-client" {
						var clientset *kubernetes.Clientset
						f.server.handlers.clientset = clientset
					}
					f.review = func(review *authorizationv1.SubjectAccessReview) error {
						f.requireIdentity(t, review.Spec)
						require.Equal(t, route.permission, *review.Spec.ResourceAttributes)
						review.Status.Allowed = decision == "allowed"
						switch decision {
						case "review-error":
							return errors.New("synthetic-private-authorizer-error")
						case "evaluation-error":
							review.Status.Allowed = true
							review.Status.EvaluationError = "synthetic-private-authorizer-error"
						case "explicit-denied":
							review.Status.Allowed = true
							review.Status.Denied = true
						}
						return nil
					}
					before := f.changes(t)
					status, body, headers := remediationHTTPCall(t, f.server.app, route.method, route.path, route.body)
					require.Equal(t, "private, no-store", headers.Get("Cache-Control"))
					require.NotContains(t, body, "synthetic-private-authorizer-error")
					require.NotContains(t, body, "synthetic-fixture-")
					require.NotContains(t, body, "negative quantity")
					require.Equal(t, 1, f.tokenReviews)
					require.Zero(t, f.kubeCalls)
					require.Zero(t, f.externalCalls.Load())
					require.Equal(t, before, f.changes(t))
					if decision == "allowed" {
						require.Equal(t, route.status, status, body)
						require.Len(t, service.calls, 1)
						require.Equal(t, route.operation, service.calls[0].operation)
						require.Equal(t, "default", service.calls[0].namespace)
						require.Equal(t, "request-current", service.calls[0].id)
						require.Len(t, f.reviews, 2)
					} else {
						require.Equal(t, http.StatusForbidden, status, body)
						require.Empty(t, service.calls)
					}
				})
			}
		})
	}
}

func TestRemediationServerConfigurationGate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := newExternalAuthorizationFixture(t)
			f.review = func(review *authorizationv1.SubjectAccessReview) error {
				review.Status.Allowed = true
				return nil
			}
			service := newRemediationServiceStub()
			cfg := ServerConfig{Clientset: f.clientset, WatchNamespace: "default"}
			if enabled {
				cfg.RemediationService = service
			}
			server := NewServer(f.kube, nil, cfg)
			status, body, headers := remediationHTTPCall(t, server.app, http.MethodPost,
				"/api/v1/remediations", remediationRequestJSON(t, remediationTestRequest()))
			require.Equal(t, "private, no-store", headers.Get("Cache-Control"))
			if enabled {
				require.Equal(t, http.StatusAccepted, status, body)
				require.Len(t, service.calls, 1)
			} else {
				require.Equal(t, http.StatusNotFound, status, body)
				require.Empty(t, service.calls)
			}
		})
	}
}

func TestRemediationDoesNotExposeLegacyValidationAPI(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	service := newRemediationServiceStub()
	f.server.handlers.remediationService = service
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/validations"},
		{http.MethodGet, "/api/v1/validations/request-one"},
		{http.MethodPost, "/api/v1/validations/request-one/cancel"},
		{http.MethodGet, "/api/v1/validations/request-one/evidence"},
	} {
		status, _ := f.request(t, route.method, route.path, "")
		require.Equal(t, http.StatusNotFound, status)
	}
	require.Empty(t, service.calls)
	require.Empty(t, f.reviews)
	require.Zero(t, f.kubeCalls)
	require.Zero(t, f.externalCalls.Load())
}

func TestRemediationDisabledAndAuthenticationGates(t *testing.T) {
	var typedNil *remediationServiceStub
	for _, tc := range []struct {
		name    string
		service contract.Service
		user    *UserInfo
		status  int
	}{
		{"disabled", nil, remediationTestUser(), http.StatusNotFound},
		{"typed-nil", typedNil, remediationTestUser(), http.StatusNotFound},
		{"unauthenticated", newRemediationServiceStub(), nil, http.StatusUnauthorized},
		{"oidc", newRemediationServiceStub(), &UserInfo{AuthType: AuthTypeOIDC, Username: "caller"}, http.StatusForbidden},
		{"context-token", newRemediationServiceStub(), &UserInfo{AuthType: "contextToken", Username: "caller"}, http.StatusForbidden},
		{"unknown-auth", newRemediationServiceStub(), &UserInfo{AuthType: "future", Username: "caller"}, http.StatusForbidden},
		{"empty-subject", newRemediationServiceStub(), &UserInfo{AuthType: AuthTypeTokenReview, Username: " "}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReviewSpec
			h := NewHandlers(HandlersConfig{RemediationService: tc.service, KubeClient: remediationAllowClient(&reviews)})
			for _, route := range remediationRouteCases(t) {
				status, _, headers := remediationHTTPCall(t, remediationHandlerApp(h, tc.user), route.method, route.path, route.body)
				require.Equal(t, tc.status, status)
				require.Equal(t, "private, no-store", headers.Get("Cache-Control"))
			}
			require.Empty(t, reviews)
			if stub, ok := tc.service.(*remediationServiceStub); ok && stub != nil {
				require.Empty(t, stub.calls)
			}
		})
	}
}

func TestRemediationNamespacesAndRunIDsAreExact(t *testing.T) {
	for _, query := range []string{
		"?namespace=", "?namespace=default&namespace=other", "?namespace=default%20",
		"?namespace=OTHER", "?namespace=other", "?Namespace=other", "?namespace=default&Namespace=other",
	} {
		t.Run(query, func(t *testing.T) {
			service := newRemediationServiceStub()
			var reviews []authorizationv1.SubjectAccessReviewSpec
			h := NewHandlers(HandlersConfig{
				WatchNamespace: "default", RemediationService: service, KubeClient: remediationAllowClient(&reviews),
			})
			status, _, _ := remediationHTTPCall(t, remediationHandlerApp(h, remediationTestUser()),
				http.MethodPost, "/api/v1/remediations"+query, remediationRequestJSON(t, remediationTestRequest()))
			require.Equal(t, http.StatusForbidden, status)
			require.Empty(t, service.calls)
			require.Empty(t, reviews)
		})
	}
	for _, id := range []string{"INVALID", "request%20", strings.Repeat("r", 64), "request..other"} {
		t.Run(id, func(t *testing.T) {
			service := newRemediationServiceStub()
			var reviews []authorizationv1.SubjectAccessReviewSpec
			h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
			status, _, _ := remediationHTTPCall(t, remediationHandlerApp(h, remediationTestUser()),
				http.MethodGet, "/api/v1/remediations/"+id, "")
			require.Equal(t, http.StatusBadRequest, status)
			require.Empty(t, service.calls)
		})
	}
	service := newRemediationServiceStub()
	service.status.Namespace = "team-a"
	var reviews []authorizationv1.SubjectAccessReviewSpec
	h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
	status, _, _ := remediationHTTPCall(t, remediationHandlerApp(h, remediationTestUser()),
		http.MethodGet, "/api/v1/remediations/request-current?namespace=team-a", "")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "team-a", service.calls[0].namespace)
	require.Equal(t, "team-a", reviews[0].ResourceAttributes.Namespace)
}

func TestRemediationInputValidationBeforeSubmission(t *testing.T) {
	base := remediationTestRequest()
	baseJSON := remediationRequestJSON(t, base)
	password := "SYNTHETIC_PASSWORD_" + rand.Text()
	credentials := url.UserPassword("SYNTHETIC_USER_"+rand.Text(), password)
	cases := map[string]string{
		"not-object":           `[]`,
		"null":                 `null`,
		"trailing-json":        baseJSON + `{}`,
		"unknown-field":        strings.TrimSuffix(baseJSON, "}") + `,"agent":"not-allowed"}`,
		"duplicate-request-id": strings.TrimSuffix(baseJSON, "}") + `,"requestID":"other"}`,
		"case-colliding-id":    strings.TrimSuffix(baseJSON, "}") + `,"RequestID":"other"}`,
		"unsupported-mode":     strings.Replace(baseJSON, `"generate"`, `"retry"`, 1),
		"missing-mode":         strings.Replace(baseJSON, `"mode":"generate",`, "", 1),
		"missing-id":           strings.Replace(baseJSON, `"requestID":"request-current",`, "", 1),
		"missing-repository":   strings.Replace(baseJSON, `"repository":"https://github.com/example/synthetic",`, "", 1),
		"missing-ref":          strings.Replace(baseJSON, `"ref":"v1.0.0"`, `"ref":""`, 1),
		"body-namespace":       strings.TrimSuffix(baseJSON, "}") + `,"namespace":"other"}`,
		"oversized-body":       strings.Repeat(" ", maxRemediationRequestBytes+1),
		"invalid-utf8":         string([]byte{'{', '"', 0xff, '"', ':', '0', '}'}),
		"supplied-patch-mode":  strings.TrimSuffix(strings.Replace(baseJSON, `"generate"`, `"verify"`, 1), "}") + `,"patch":"candidate"}`,
		"generate-with-patch":  strings.TrimSuffix(baseJSON, "}") + `,"patch":"candidate"}`,
		"validate-with-patch":  strings.TrimSuffix(strings.Replace(baseJSON, `"generate"`, `"validate"`, 1), "}") + `,"patch":"candidate"}`,
		"empty-patch-field":    strings.TrimSuffix(baseJSON, "}") + `,"patch":""}`,
		"null-patch-field":     strings.TrimSuffix(baseJSON, "}") + `,"patch":null}`,
	}
	for _, tc := range []struct {
		name string
		edit func(*contract.Request)
	}{
		{"unsupported-verify-mode", func(r *contract.Request) { r.Mode = contract.Mode("verify") }},
		{"oversized-report", func(r *contract.Request) {
			r.Report = json.RawMessage(`{"title":"Synthetic","problem":"` + strings.Repeat("x", maxRemediationReportBytes) + `"}`)
		}},
		{"report-missing-title", func(r *contract.Request) { r.Report = json.RawMessage(`{"problem":"Synthetic"}`) }},
		{"report-missing-problem", func(r *contract.Request) { r.Report = json.RawMessage(`{"title":"Synthetic"}`) }},
		{"report-duplicate-key", func(r *contract.Request) {
			r.Report = json.RawMessage(`{"title":"Synthetic","title":"Other","problem":"Synthetic"}`)
		}},
		{"conflicting-repository", func(r *contract.Request) {
			r.Report = json.RawMessage(`{"title":"Synthetic","problem":"Synthetic","repository":"https://github.com/example/other"}`)
		}},
		{"conflicting-ref", func(r *contract.Request) {
			r.Report = json.RawMessage(`{"title":"Synthetic","problem":"Synthetic","ref":"other"}`)
		}},
		{"credentials-in-repository", func(r *contract.Request) {
			r.Repository = (&url.URL{
				Scheme: "https",
				Host:   "github.com",
				Path:   "/example/synthetic",
				User:   credentials,
			}).String()
		}},
		{"ref-whitespace", func(r *contract.Request) { r.Ref = " v1.0.0" }},
	} {
		request := base
		tc.edit(&request)
		cases[tc.name] = remediationRequestJSON(t, request)
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			service := newRemediationServiceStub()
			var reviews []authorizationv1.SubjectAccessReviewSpec
			h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
			status, result, headers := remediationHTTPCall(t, remediationHandlerApp(h, remediationTestUser()), http.MethodPost, "/api/v1/remediations", body)
			expected := http.StatusBadRequest
			if name == "oversized-body" {
				expected = http.StatusRequestEntityTooLarge
			}
			require.Equal(t, expected, status, result)
			require.Empty(t, service.calls)
			require.NotContains(t, result, "Synthetic")
			require.NotContains(t, result, credentials.Username())
			require.NotContains(t, result, password)
			require.Equal(t, "private, no-store", headers.Get("Cache-Control"))
		})
	}
}

func TestRemediationModesAndIdentityPreserved(t *testing.T) {
	for _, mode := range []contract.Mode{contract.Generate, contract.Validate} {
		t.Run(string(mode), func(t *testing.T) {
			service := newRemediationServiceStub()
			var reviews []authorizationv1.SubjectAccessReviewSpec
			h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
			app := remediationHandlerApp(h, remediationTestUser())
			request := remediationTestRequest()
			request.Mode = mode
			body := remediationRequestJSON(t, request)
			for range 2 {
				status, result, _ := remediationHTTPCall(t, app, http.MethodPost, "/api/v1/remediations", body)
				require.Equal(t, http.StatusAccepted, status, result)
				var got contract.Status
				require.NoError(t, json.Unmarshal([]byte(result), &got))
				require.Equal(t, request.RequestID, got.ID)
			}
			require.Len(t, service.calls, 2)
			for _, call := range service.calls {
				require.Equal(t, request, call.request)
				require.Equal(t, remediationTestUser().Username, call.subject)
				require.Equal(t, "default", call.namespace)
				require.Equal(t, request.RequestID, call.id)
			}
		})
	}
}

func TestRemediationRequestEncodedBoundaries(t *testing.T) {
	request := remediationTestRequest()
	emptyReport := `{"title":"Synthetic","problem":""}`
	request.Report = json.RawMessage(`{"title":"Synthetic","problem":"` + strings.Repeat("<", maxRemediationReportBytes-len(emptyReport)) + `"}`)
	body := remediationRequestJSON(t, request)
	require.LessOrEqual(t, len(body), maxRemediationRequestBytes)
	// JSON escaping counts toward the report wire limit, not just decoded text.
	_, err := decodeRemediationRequest([]byte(body))
	require.ErrorIs(t, err, contract.ErrInvalid)
	request.Report = json.RawMessage(`{"title":"Synthetic","problem":"` + strings.Repeat("x", maxRemediationReportBytes-len(emptyReport)) + `"}`)
	body = remediationRequestJSON(t, request)
	got, err := decodeRemediationRequest([]byte(body))
	require.NoError(t, err)
	require.Len(t, got.Report, maxRemediationReportBytes)
}

type remediationCountingReader struct {
	bytes int
}

func (r *remediationCountingReader) Read(content []byte) (int, error) {
	for i := range content {
		content[i] = 'x'
	}
	r.bytes += len(content)
	return len(content), nil
}

func TestRemediationStreamingReadIsBoundedAfterAuthorization(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			service := newRemediationServiceStub()
			var reviews []authorizationv1.SubjectAccessReviewSpec
			h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
			user := remediationTestUser()
			if !allowed {
				user.AuthType = AuthTypeOIDC
			}
			reader := &remediationCountingReader{}
			app := remediationHandlerApp(h, user, func(c fiber.Ctx) error {
				c.Request().SetBodyStream(reader, -1)
				return c.Next()
			})
			status, _, _ := remediationHTTPCall(t, app, http.MethodPost, "/api/v1/remediations", "")
			require.Empty(t, service.calls)
			if allowed {
				require.Equal(t, http.StatusRequestEntityTooLarge, status)
				require.Equal(t, maxRemediationRequestBytes+1, reader.bytes)
			} else {
				require.Equal(t, http.StatusForbidden, status)
				require.Zero(t, reader.bytes)
			}
		})
	}
}

func TestRemediationRequestHeaderLimits(t *testing.T) {
	for _, path := range []string{"/api/v1/remediations", "/api/v1/remediations/request-one/cancel", "/API/V1/REMEDIATIONS/?namespace=default"} {
		var header fasthttp.RequestHeader
		header.SetMethod(http.MethodPost)
		header.SetRequestURI(path)
		config := requestBodyConfig(&header)
		require.Equal(t, maxRemediationRequestBytes, config.MaxRequestBodySize)
		require.Equal(t, 30*time.Second, config.ReadTimeout)
	}
}

func TestRemediationErrorsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{contract.ErrDisabled, http.StatusNotFound},
		{contract.ErrInvalid, http.StatusBadRequest},
		{contract.ErrEvidenceScopeUnsupported, http.StatusUnprocessableEntity},
		{contract.ErrNotFound, http.StatusNotFound},
		{contract.ErrConflict, http.StatusConflict},
		{contract.ErrUnavailable, http.StatusServiceUnavailable},
		{contract.ErrForbidden, http.StatusForbidden},
		{errors.New("unexpected"), http.StatusInternalServerError},
	} {
		t.Run(fmt.Sprint(tc.status, tc.err), func(t *testing.T) {
			for _, route := range remediationRouteCases(t) {
				service := newRemediationServiceStub()
				service.err = fmt.Errorf("synthetic-private-report-and-credential: %w", tc.err)
				var reviews []authorizationv1.SubjectAccessReviewSpec
				h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
				status, body, headers := remediationHTTPCall(t, remediationHandlerApp(h, remediationTestUser()), route.method, route.path, route.body)
				require.Equal(t, tc.status, status)
				require.NotContains(t, body, "synthetic-private")
				require.Equal(t, "private, no-store", headers.Get("Cache-Control"))
			}
		})
	}
}

func TestRemediationRejectsInvalidServiceStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*contract.Status)
	}{
		{"different-id", func(s *contract.Status) { s.ID = "other" }},
		{"different-namespace", func(s *contract.Status) { s.Namespace = "other" }},
		{"unknown-phase", func(s *contract.Status) { s.Phase = "synthetic-private-report" }},
		{"unknown-stage", func(s *contract.Status) { s.Stage = "synthetic-private-report" }},
		{"unknown-outcome", func(s *contract.Status) { s.Outcome = "synthetic-private-report" }},
		{"unknown-evidence-scope", func(s *contract.Status) { s.EvidenceScope = "synthetic-private-report" }},
		{"raw-reason", func(s *contract.Status) { s.Reason = "synthetic-private-report\nraw response" }},
		{"premature-verified", func(s *contract.Status) { s.Outcome = contract.OutcomeVerified }},
		{"half-task-reference", func(s *contract.Status) { s.TaskName = "parent" }},
		{"unsafe-task-reference", func(s *contract.Status) { s.TaskName, s.TaskUID = "../parent", "parent-uid" }},
		{"unsafe-task-uid", func(s *contract.Status) { s.TaskName, s.TaskUID = "parent", "synthetic-private\nvalue" }},
		{"nil", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newRemediationServiceStub()
			if tc.edit == nil {
				service.status = nil
			} else {
				tc.edit(service.status)
			}
			var reviews []authorizationv1.SubjectAccessReviewSpec
			h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
			status, body, _ := remediationHTTPCall(t, remediationHandlerApp(h, remediationTestUser()), http.MethodGet, "/api/v1/remediations/request-current", "")
			require.Equal(t, http.StatusServiceUnavailable, status)
			require.NotContains(t, body, "synthetic-private")
		})
	}
}

func TestRemediationEvidenceRequirementIsPreservedOrRejected(t *testing.T) {
	for _, mode := range []contract.Mode{contract.Generate, contract.Validate} {
		for _, tc := range []struct {
			name                string
			required, supported contract.EvidenceScope
			err                 error
			status              int
		}{
			{"source harness", contract.SourceHarness, contract.SourceHarness, nil, http.StatusAccepted},
			{"strict runtime", contract.ApplicationRuntime, "", contract.ErrEvidenceScopeUnsupported, http.StatusUnprocessableEntity},
			{"downgrade refused", contract.ApplicationRuntime, contract.SourceHarness, nil, http.StatusServiceUnavailable},
			{"unrecorded scope refused", contract.SourceHarness, "", nil, http.StatusServiceUnavailable},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				service := newRemediationServiceStub()
				service.status.EvidenceScope, service.err = tc.supported, tc.err
				var reviews []authorizationv1.SubjectAccessReviewSpec
				h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
				request := remediationTestRequest()
				request.Mode, request.RequiredEvidenceScope = mode, tc.required
				status, body, headers := remediationHTTPCall(t, remediationHandlerApp(h, remediationTestUser()),
					http.MethodPost, "/api/v1/remediations", remediationRequestJSON(t, request))
				require.Equal(t, tc.status, status)
				require.Equal(t, "private, no-store", headers.Get("Cache-Control"))
				require.Len(t, service.calls, 1)
				require.Equal(t, tc.required, service.calls[0].request.RequiredEvidenceScope)
				if tc.status == http.StatusAccepted {
					require.Contains(t, body, `"evidenceScope":"source-harness"`)
				}
				if tc.status == http.StatusUnprocessableEntity {
					require.Contains(t, body, contract.ErrEvidenceScopeUnsupported.Error())
				}
			})
		}
	}
}

func TestRemediationCandidateFeedbackAndPatchRemainPrivate(t *testing.T) {
	service := newRemediationServiceStub()
	service.status.Phase = contract.PhaseFailed
	service.status.Stage = contract.StageComplete
	service.status.Outcome = contract.OutcomeUnverified
	service.status.Reason = "candidate-regression"
	service.status.PatchReady = true
	var reviews []authorizationv1.SubjectAccessReviewSpec
	h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
	app := remediationHandlerApp(h, remediationTestUser())
	status, body, _ := remediationHTTPCall(t, app, http.MethodGet, "/api/v1/remediations/request-current", "")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"outcome":"Unverified"`)
	require.Contains(t, body, `"reason":"candidate-regression"`)
	status, body, headers := remediationHTTPCall(t, app, http.MethodGet, "/api/v1/remediations/request-current/patch", "")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(service.patch), body)
	require.Equal(t, `attachment; filename="candidate.patch"`, headers.Get("Content-Disposition"))
	require.Equal(t, "application/octet-stream", headers.Get("Content-Type"))
	require.Equal(t, "private, no-store", headers.Get("Cache-Control"))
	require.Equal(t, "nosniff", headers.Get("X-Content-Type-Options"))
	require.Len(t, service.calls, 2)
}

func TestRemediationPatchAndCancelFailClosed(t *testing.T) {
	for _, patch := range [][]byte{nil, bytes.Repeat([]byte("x"), maxRemediationPatchBytes+1), {0xff}} {
		service := newRemediationServiceStub()
		service.patch = patch
		var reviews []authorizationv1.SubjectAccessReviewSpec
		h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
		status, _, _ := remediationHTTPCall(t, remediationHandlerApp(h, remediationTestUser()), http.MethodGet, "/api/v1/remediations/request-current/patch", "")
		require.Equal(t, http.StatusServiceUnavailable, status)
	}
	service := newRemediationServiceStub()
	var reviews []authorizationv1.SubjectAccessReviewSpec
	h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
	status, _, _ := remediationHTTPCall(t, remediationHandlerApp(h, remediationTestUser()), http.MethodPost, "/api/v1/remediations/request-current/cancel", `{"newRequestID":"other"}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Empty(t, service.calls)
}

func TestRemediationNeverLogsReportOrCredential(t *testing.T) {
	var logs bytes.Buffer
	previous := log
	log = funcr.New(func(prefix, args string) { _, _ = fmt.Fprintln(&logs, prefix, args) }, funcr.Options{})
	t.Cleanup(func() { log = previous })
	service := newRemediationServiceStub()
	service.err = errors.New("synthetic-private-service-error")
	var reviews []authorizationv1.SubjectAccessReviewSpec
	h := NewHandlers(HandlersConfig{RemediationService: service, KubeClient: remediationAllowClient(&reviews)})
	app := remediationHandlerApp(h, remediationTestUser())
	request := remediationTestRequest()
	request.Report = json.RawMessage(`{"title":"Synthetic","problem":"synthetic-private-problem"}`)
	status, body, _ := remediationHTTPCall(t, app, http.MethodPost, "/api/v1/remediations", remediationRequestJSON(t, request))
	require.Equal(t, http.StatusInternalServerError, status)
	require.NotContains(t, logs.String()+body, "synthetic-private")
	require.NotContains(t, logs.String()+body, "synthetic-fixture-")
	require.NotEmpty(t, logs.String())
}

func TestRemediationAggregateUsesOrdinaryTaskResultAuthorization(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			f := newExternalAuthorizationFixture(t)
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "remediation-parent", Namespace: "default", UID: "parent-uid",
					Annotations: map[string]string{"private-input": "synthetic-private-report"}},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
				Status: corev1alpha1.TaskStatus{ResultRef: &corev1alpha1.ResultReference{
					Available: true,
				}},
			}
			require.NoError(t, f.kube.Create(t.Context(), task))
			result, err := common.FormatStructuredResult(&common.StructuredResult{
				Version: 1, Summary: "One candidate was evaluated.", Verdict: contract.OutcomeUnverified,
				Feedback: "candidate regression check failed", Diff: string(newRemediationServiceStub().patch),
			})
			require.NoError(t, err)
			require.NoError(t, f.store.SaveResult(t.Context(), "default", task.Name, result))
			f.kubeCalls = 0
			f.review = func(review *authorizationv1.SubjectAccessReview) error {
				f.requireIdentity(t, review.Spec)
				require.Equal(t, authorizationv1.ResourceAttributes{
					Namespace: "default", Group: "core.orka.ai", Resource: "tasks", Verb: "get", Name: task.Name,
				}, *review.Spec.ResourceAttributes)
				review.Status.Allowed = allowed
				return nil
			}
			before := f.changes(t)
			status, body, headers := remediationHTTPCall(t, f.server.app, http.MethodGet,
				"/api/v1/tasks/"+task.Name+"/result", "")
			require.NotContains(t, body, "synthetic-private")
			require.Equal(t, before, f.changes(t))
			require.Zero(t, f.externalCalls.Load())
			if allowed {
				require.Equal(t, http.StatusOK, status)
				require.Equal(t, "private, no-store", headers.Get("Cache-Control"))
				var response map[string]string
				require.NoError(t, json.Unmarshal([]byte(body), &response))
				require.Equal(t, string(result), response["result"])
			} else {
				require.Equal(t, http.StatusForbidden, status)
				require.NotContains(t, body, "candidate regression check failed")
				require.Zero(t, f.kubeCalls, "denial must precede the Task/result read")
			}
		})
	}
}
