package api

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"

	"github.com/gofiber/fiber/v3"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storetest "github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func nativeAPIStore(t *testing.T) *sqlite.Store {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s := sqlite.NewStore(db, ":memory:")
	cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	require.NoError(t, s.SetAgentExecutionSnapshotCipher(cipher))
	return s
}

func nativeAPIApp(t *testing.T, cfg HandlersConfig, middleware fiber.Handler) *fiber.App {
	t.Helper()
	cfg.Client = fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	h := NewHandlers(cfg)
	app := fiber.New(fiber.Config{BodyLimit: defaultAPIRequestBodyLimit, StreamRequestBody: true})
	if middleware != nil {
		app.Use(middleware)
	}
	app.Post("/sessions/:id/native", h.ImportNativeSession)
	app.Get("/sessions/:id/native", h.ExportNativeSession)
	app.Get("/sessions/:id", h.GetSession)
	return app
}

func nativeAPIRequest(t *testing.T, app *fiber.App, method, path string, value any) *http.Response {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	return response
}

func TestNativeSessionMigrationAPIImportExportAndConflict(t *testing.T) {
	s := nativeAPIStore(t)
	app := nativeAPIApp(t, HandlersConfig{SessionStore: s}, nil)
	snapshot := storetest.NativeSessionSnapshot(t, "private history")
	request := nativeSessionImportRequest{OperationID: "stable-operation", Data: snapshot.Data}
	resp := nativeAPIRequest(t, app, http.MethodPost, "/sessions/imported/native", request)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var receipt store.NativeSessionImportReceipt
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&receipt))
	_ = resp.Body.Close()
	resp = nativeAPIRequest(t, app, http.MethodPost, "/sessions/imported/native", request)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var retry store.NativeSessionImportReceipt
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&retry))
	_ = resp.Body.Close()
	require.Equal(t, receipt, retry)
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/sessions/imported/native", nil), fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var export nativeSessionExportResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&export))
	_ = resp.Body.Close()
	require.Equal(t, snapshot.Data, export.Data)
	require.Equal(t, snapshot.DataDigest, export.DataDigest)
	request.Data = storetest.NativeSessionSnapshot(t, "changed private history").Data
	resp = nativeAPIRequest(t, app, http.MethodPost, "/sessions/imported/native", request)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	_ = resp.Body.Close()
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/sessions/imported", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var ordinary map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&ordinary))
	_ = resp.Body.Close()
	encoded, err := json.Marshal(ordinary)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), base64.StdEncoding.EncodeToString(snapshot.Data))
	require.NotContains(t, string(encoded), "dataDigest")
	require.NotContains(t, string(encoded), "providerSessionID")
	require.NoError(t, s.CreateSession(t.Context(), &store.SessionRecord{Namespace: "default", Name: "gateway", SessionType: store.SessionTypeGateway}))
	request.Data = snapshot.Data
	resp = nativeAPIRequest(t, app, http.MethodPost, "/sessions/gateway/native", request)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	_ = resp.Body.Close()
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/sessions/gateway/native", nil), fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	_ = resp.Body.Close()
}

func TestNativeSessionMigrationAPIRejectsChatBeforeProviderInvocation(t *testing.T) {
	const providerType = "native-import-chat-admission-test"
	provider := &chatMockProvider{name: providerType}
	llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) {
		return provider, nil
	})
	s := nativeAPIStore(t)
	app := nativeAPIApp(t, HandlersConfig{SessionStore: s}, nil)
	client := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(
		providerCRD("default", "default", providerType, "test-model")...,
	).Build()
	config := DefaultChatConfig()
	config.Provider = "default"
	chat := newTestChatHandler(t, client, s, s, config)
	app.Post("/api/v1/chat", chat.HandleChat)
	snapshot := storetest.NativeSessionSnapshot(t, "private imported history")
	response := nativeAPIRequest(t, app, http.MethodPost, "/sessions/imported/native", nativeSessionImportRequest{
		OperationID: "import-operation", Data: snapshot.Data,
	})
	require.Equal(t, http.StatusCreated, response.StatusCode)
	_ = response.Body.Close()

	body, err := json.Marshal(ChatRequest{Message: "unrelated chat", SessionID: "imported", Namespace: "default"})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err = app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode)
	_ = response.Body.Close()
	require.Zero(t, provider.callCount)

	session, err := s.GetSession(t.Context(), "default", "imported")
	require.NoError(t, err)
	require.Zero(t, session.MessageCount)
	require.Empty(t, session.Messages)
	response, err = app.Test(httptest.NewRequest(http.MethodGet, "/sessions/imported/native", nil), fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var exported nativeSessionExportResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&exported))
	_ = response.Body.Close()
	require.Equal(t, snapshot.Data, exported.Data)
	require.Equal(t, snapshot.DataDigest, exported.DataDigest)
}

func TestNativeSessionMigrationAPISessionRBAC(t *testing.T) {
	for _, deny := range []string{"create", "get", ""} {
		t.Run("deny-"+deny, func(t *testing.T) {
			s := nativeAPIStore(t)
			kube := kubefake.NewClientset()
			var verbs []string
			kube.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
				attrs := review.Spec.ResourceAttributes
				require.Equal(t, "sessions", attrs.Resource)
				require.Equal(t, "default", attrs.Namespace)
				if attrs.Verb == "get" {
					require.Equal(t, "authorized-session", attrs.Name)
				} else {
					require.Empty(t, attrs.Name)
				}
				verbs = append(verbs, attrs.Verb)
				review.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: attrs.Verb != deny}
				return true, review, nil
			})
			app := nativeAPIApp(t, HandlersConfig{SessionStore: s, KubeClient: kube}, tokenReviewUserMiddleware(limitedTokenReviewUser("default")))
			snapshot := storetest.NativeSessionSnapshot(t, "private history")
			resp := nativeAPIRequest(t, app, http.MethodPost, "/sessions/authorized-session/native", nativeSessionImportRequest{OperationID: "operation", Data: snapshot.Data})
			_ = resp.Body.Close()
			if deny != "" {
				require.Equal(t, http.StatusForbidden, resp.StatusCode)
				_, err := s.GetSession(t.Context(), "default", "authorized-session")
				require.ErrorIs(t, err, store.ErrNotFound)
			} else {
				require.Equal(t, http.StatusCreated, resp.StatusCode)
				require.Equal(t, []string{"create", "get"}, verbs)
			}
		})
	}
	s := nativeAPIStore(t)
	snapshot := storetest.NativeSessionSnapshot(t, "export private history")
	request := store.NativeSessionImport{Namespace: "default", SessionName: "authorized-session", OperationID: "operation", RequestDigest: store.NativeSessionImportDigest("default", "authorized-session", snapshot.DataDigest), Snapshot: snapshot}
	request.Snapshot.RuntimeSessionUID = ""
	request.Snapshot.RuntimeProfileDigest = ""
	request.Snapshot.WorkingDirectory = ""
	_, err := s.StageNativeSessionImport(t.Context(), request)
	require.NoError(t, err)
	app := nativeAPIApp(t, HandlersConfig{SessionStore: s, KubeClient: denyingSubjectAccessReviewClient(t, nil, nil)}, tokenReviewUserMiddleware(limitedTokenReviewUser("default")))
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/sessions/authorized-session/native", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	_ = resp.Body.Close()
}

func TestNativeSessionMigrationAPIContextScopesAndNamespace(t *testing.T) {
	for _, tt := range []struct {
		name, method, path string
		scopes             []string
		namespace          string
	}{
		{name: "import needs write", method: http.MethodPost, path: "/sessions/new/native", scopes: []string{ContextTokenScopeSessionsRead}},
		{name: "export needs read", method: http.MethodGet, path: "/sessions/new/native", scopes: []string{ContextTokenScopeSessionsWrite}},
		{name: "namespace constrained", method: http.MethodPost, path: "/sessions/new/native?namespace=other", scopes: []string{ContextTokenScopeSessionsWrite}, namespace: "default"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := nativeAPIStore(t)
			app := nativeAPIApp(t, HandlersConfig{SessionStore: s, ContextTokenAuthorization: enforceContextTokenAuthorizationConfig()}, func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeContextToken, ContextToken: &ContextToken{Scopes: tt.scopes, TransactionContext: map[string]any{"namespace": tt.namespace}}})
				return c.Next()
			})
			resp := nativeAPIRequest(t, app, tt.method, tt.path, nativeSessionImportRequest{OperationID: "operation", Data: []byte("invalid")})
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			_ = resp.Body.Close()
			sessions, err := s.ListSessions(t.Context(), "default")
			require.NoError(t, err)
			require.Empty(t, sessions)
		})
	}
}

func TestNativeSessionMigrationAPIRejectsMalformedBundles(t *testing.T) {
	s := nativeAPIStore(t)
	app := nativeAPIApp(t, HandlersConfig{SessionStore: s}, nil)
	for _, data := range [][]byte{nil, []byte(`{"format":"unsupported"}`), bytes.Repeat([]byte{'x'}, maxNativeSessionBundleBytes+1)} {
		resp := nativeAPIRequest(t, app, http.MethodPost, "/sessions/invalid/native", nativeSessionImportRequest{OperationID: "operation", Data: data})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		_ = resp.Body.Close()
	}
	sessions, err := s.ListSessions(t.Context(), "default")
	require.NoError(t, err)
	require.Empty(t, sessions)
}

type nativeSessionCountingReader struct {
	io.Reader
	bytesRead int
}

func (r *nativeSessionCountingReader) Read(data []byte) (int, error) {
	n, err := r.Reader.Read(data)
	r.bytesRead += n
	return n, err
}

func TestNativeSessionMigrationAPIBoundsStreamedRequests(t *testing.T) {
	limit := harnessv2.NativeSessionJSONLimit(maxNativeSessionBundleBytes)
	for _, chunked := range []bool{false, true} {
		name := "content-length"
		if chunked {
			name = "chunked"
		}
		t.Run(name, func(t *testing.T) {
			s := nativeAPIStore(t)
			reader := &nativeSessionCountingReader{Reader: bytes.NewReader(bytes.Repeat([]byte{' '}, 2*limit))}
			app := nativeAPIApp(t, HandlersConfig{SessionStore: s}, func(c fiber.Ctx) error {
				length := 2 * limit
				if chunked {
					length = -1
				}
				c.Request().SetBodyStream(reader, length)
				return c.Next()
			})
			response, err := app.Test(httptest.NewRequest(http.MethodPost, "/sessions/oversized/native", nil))
			require.NoError(t, err)
			require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
			_ = response.Body.Close()
			if chunked {
				require.Equal(t, limit+1, reader.bytesRead)
			} else {
				require.Zero(t, reader.bytesRead)
			}
			_, err = s.GetSession(t.Context(), "default", "oversized")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestNativeSessionMigrationAPIAcceptsChunkedRequest(t *testing.T) {
	s := nativeAPIStore(t)
	snapshot := storetest.NativeSessionSnapshot(t, "chunked native history")
	body, err := json.Marshal(nativeSessionImportRequest{OperationID: "chunked-operation", Data: snapshot.Data})
	require.NoError(t, err)
	reader := &nativeSessionCountingReader{Reader: bytes.NewReader(body)}
	app := nativeAPIApp(t, HandlersConfig{SessionStore: s}, func(c fiber.Ctx) error {
		c.Request().SetBodyStream(reader, -1)
		return c.Next()
	})
	response, err := app.Test(httptest.NewRequest(http.MethodPost, "/sessions/chunked/native", nil), fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	_ = response.Body.Close()
	require.Equal(t, len(body), reader.bytesRead)
	response, err = app.Test(httptest.NewRequest(http.MethodGet, "/sessions/chunked/native", nil), fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var export nativeSessionExportResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&export))
	_ = response.Body.Close()
	require.Equal(t, snapshot.Data, export.Data)
}

func TestNativeSessionMigrationAPIRejectsUnfinishedOversizedChunk(t *testing.T) {
	s := nativeAPIStore(t)
	app := nativeAPIApp(t, HandlersConfig{SessionStore: s}, nil)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() {
		_ = app.Shutdown()
		<-serveErrors
	})
	connection, err := net.Dial("tcp4", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	require.NoError(t, connection.SetDeadline(time.Now().Add(5*time.Second)))
	oversized := harnessv2.NativeSessionJSONLimit(maxNativeSessionBundleBytes) + 1
	_, err = fmt.Fprintf(connection, "POST /sessions/oversized/native HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n", 2*oversized)
	require.NoError(t, err)
	_, err = connection.Write(bytes.Repeat([]byte{' '}, oversized))
	require.NoError(t, err)
	// Do not finish the chunk or request. The route must return at its limit.
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
	_ = response.Body.Close()
	_, err = s.GetSession(t.Context(), "default", "oversized")
	require.ErrorIs(t, err, store.ErrNotFound)
}
