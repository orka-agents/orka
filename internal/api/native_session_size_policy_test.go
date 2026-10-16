package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func nativePolicyData(t *testing.T, size int) []byte {
	t.Helper()
	snapshot := storetest.NativeSessionSnapshot(t, "private native policy fixture")
	require.LessOrEqual(t, len(snapshot.Data), size)
	return append(bytes.Clone(snapshot.Data), bytes.Repeat([]byte{' '}, size-len(snapshot.Data))...)
}

func nativePolicyServer(t *testing.T, s *sqlite.Store, limit int) (*Server, string, *http.Client, string) {
	t.Helper()
	kube := kubefake.NewClientset()
	kube.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})
	const token = "native-size-policy-test-only-token"
	server := NewServer(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:default:native-policy"}), nil, ServerConfig{
		SessionStore: s, Clientset: kube, WatchNamespace: "default", NativeSessionMaxBytes: limit,
	})
	base, client := nativeRetryStartHTTP(t, server.app)
	return server, base, client, token
}

func nativePolicyHTTPRequest(t *testing.T, client *http.Client, method, target, token string, body io.Reader) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, target, body)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	require.NoError(t, err)
	return response
}

func TestNativeSessionSizePolicyConfiguredImportExport(t *testing.T) {
	for _, limit := range []int{640 << 10, 9 << 20} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			s := nativeAPIStore(t)
			require.NoError(t, s.SetNativeSessionMaxBytes(limit))
			server, base, client, token := nativePolicyServer(t, s, limit)
			require.Equal(t, limit, server.handlers.nativeSessionMaxBytes)
			data := nativePolicyData(t, limit)
			body, err := json.Marshal(nativeSessionImportRequest{OperationID: "exact-custom-limit", Data: data})
			require.NoError(t, err)
			require.Less(t, len(body), harnessv2.NativeSessionJSONLimit(limit))
			response := nativePolicyHTTPRequest(t, client, http.MethodPost, base+"/api/v1/sessions/exact/native", token, bytes.NewReader(body))
			var receipt store.NativeSessionImportReceipt
			nativeRetryReadResponse(t, response, http.StatusCreated, &receipt)
			response = nativePolicyHTTPRequest(t, client, http.MethodGet, base+"/api/v1/sessions/exact/native", token, nil)
			var exported nativeSessionExportResponse
			nativeRetryReadResponse(t, response, http.StatusOK, &exported)
			require.Equal(t, data, exported.Data)
			require.Equal(t, receipt.DataDigest, exported.DataDigest)

			body, err = json.Marshal(nativeSessionImportRequest{OperationID: "over-custom-limit", Data: append(bytes.Clone(data), ' ')})
			require.NoError(t, err)
			response = nativePolicyHTTPRequest(t, client, http.MethodPost, base+"/api/v1/sessions/over/native", token, bytes.NewReader(body))
			nativeRetryReadResponse(t, response, http.StatusBadRequest, nil)
			_, err = s.GetSession(t.Context(), "default", "over")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestNativeSessionSizePolicyExportLoweredCapRetainsStoredData(t *testing.T) {
	const limit = 640 << 10
	s := nativeAPIStore(t)
	app := nativeAPIApp(t, HandlersConfig{SessionStore: s, NativeSessionMaxBytes: limit}, nil)
	data := nativePolicyData(t, limit)
	response := nativeAPIRequest(t, app, http.MethodPost, "/sessions/retained/native", nativeSessionImportRequest{OperationID: "retained", Data: data})
	nativeRetryReadResponse(t, response, http.StatusCreated, nil)
	before, err := s.GetNativeSession(t.Context(), "default", "retained", "")
	require.NoError(t, err)
	require.NoError(t, s.SetNativeSessionMaxBytes(limit-1))
	app = nativeAPIApp(t, HandlersConfig{SessionStore: s, NativeSessionMaxBytes: limit - 1}, nil)
	response, err = app.Test(httptest.NewRequest(http.MethodGet, "/sessions/retained/native", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
	message, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Contains(t, string(message), "configured export limit")
	require.NotContains(t, string(message), "private native policy fixture")
	after, err := s.GetNativeSession(t.Context(), "default", "retained", "")
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestNativeSessionSizePolicyConstructorValidation(t *testing.T) {
	for _, limit := range []int{0, -1, harnessv2.MaxNativeSessionBytes + 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			s := nativeAPIStore(t)
			h := NewHandlers(HandlersConfig{SessionStore: s, NativeSessionMaxBytes: limit})
			server := NewServer(nil, nil, ServerConfig{SessionStore: s, NativeSessionMaxBytes: limit})
			if limit == 0 {
				require.NoError(t, h.nativeSessionConfigErr)
				require.NoError(t, server.handlers.nativeSessionConfigErr)
				require.Equal(t, harnessv2.DefaultMaxNativeSessionBytes, h.nativeSessionMaxBytes)
				require.Equal(t, h.nativeSessionMaxBytes, server.handlers.nativeSessionMaxBytes)
				return
			}
			require.Error(t, h.nativeSessionConfigErr)
			require.Error(t, server.handlers.nativeSessionConfigErr)
			require.Zero(t, h.nativeSessionMaxBytes)
			reader := &nativeSessionCountingReader{Reader: strings.NewReader("must not be read")}
			app := nativeAPIApp(t, HandlersConfig{SessionStore: s, NativeSessionMaxBytes: limit}, func(c fiber.Ctx) error {
				c.Request().SetBodyStream(reader, -1)
				return c.Next()
			})
			for _, method := range []string{http.MethodPost, http.MethodGet} {
				response, err := app.Test(httptest.NewRequest(method, "/sessions/invalid/native", nil))
				require.NoError(t, err)
				nativeRetryReadResponse(t, response, http.StatusServiceUnavailable, nil)
				require.Zero(t, reader.bytesRead)
			}
		})
	}
}

func TestNativeSessionSizePolicyAuthorizationBeforeBodyRead(t *testing.T) {
	s := nativeAPIStore(t)
	reader := &nativeSessionCountingReader{Reader: strings.NewReader("must not be read")}
	app := nativeAPIApp(t, HandlersConfig{
		SessionStore: s, NativeSessionMaxBytes: 640 << 10,
		KubeClient: denyingSubjectAccessReviewClient(t, nil, nil),
	}, func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, limitedTokenReviewUser("default"))
		c.Request().SetBodyStream(reader, -1)
		return c.Next()
	})
	response, err := app.Test(httptest.NewRequest(http.MethodPost, "/sessions/forbidden/native", nil))
	require.NoError(t, err)
	nativeRetryReadResponse(t, response, http.StatusForbidden, nil)
	require.Zero(t, reader.bytesRead)
}

func TestNativeSessionSizePolicyStreamingEnvelope(t *testing.T) {
	const limit = 9 << 20
	s := nativeAPIStore(t)
	require.NoError(t, s.SetNativeSessionMaxBytes(limit))
	server, base, client, token := nativePolicyServer(t, s, limit)
	body, err := json.Marshal(nativeSessionImportRequest{OperationID: "envelope-limit", Data: nativePolicyData(t, limit)})
	require.NoError(t, err)
	envelopeLimit := harnessv2.NativeSessionJSONLimit(limit)
	body = append(body, bytes.Repeat([]byte{' '}, envelopeLimit-len(body))...)
	require.Len(t, body, envelopeLimit)
	require.Greater(t, len(body), defaultAPIRequestBodyLimit)
	require.Greater(t, len(body), harnessv2.NativeSessionJSONLimit(harnessv2.DefaultMaxNativeSessionBytes))
	response := nativePolicyHTTPRequest(t, client, http.MethodPost, base+"/API/V1/SESSIONS/envelope/NATIVE/?namespace=default", token, io.NopCloser(bytes.NewReader(body)))
	nativeRetryReadResponse(t, response, http.StatusCreated, nil)

	connection, err := net.Dial("tcp4", strings.TrimPrefix(base, "http://"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	require.NoError(t, connection.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(connection, "POST /api/v1/sessions/oversized/native HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n", token, 2*envelopeLimit)
	require.NoError(t, err)
	_, err = connection.Write(bytes.Repeat([]byte{' '}, envelopeLimit+1))
	require.NoError(t, err)
	response, err = http.ReadResponse(bufio.NewReader(connection), nil)
	require.NoError(t, err)
	nativeRetryReadResponse(t, response, http.StatusRequestEntityTooLarge, nil)
	_, err = s.GetSession(t.Context(), "default", "oversized")
	require.ErrorIs(t, err, store.ErrNotFound)

	require.Equal(t, defaultAPIRequestBodyLimit, server.app.Config().BodyLimit)
	response = nativePolicyHTTPRequest(t, client, http.MethodPost, base+"/api/v1/tasks", token, io.NopCloser(strings.NewReader(`{}`)))
	nativeRetryReadResponse(t, response, http.StatusLengthRequired, nil)

	ordinary, err := net.Dial("tcp4", strings.TrimPrefix(base, "http://"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ordinary.Close() })
	require.NoError(t, ordinary.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(ordinary, "POST /api/v1/tasks HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", token, defaultAPIRequestBodyLimit+1)
	require.NoError(t, err)
	// fasthttp pre-reads at most 8 KiB even for fixed-length streams.
	_, err = ordinary.Write(bytes.Repeat([]byte{' '}, 8<<10))
	require.NoError(t, err)
	response, err = http.ReadResponse(bufio.NewReader(ordinary), nil)
	require.NoError(t, err)
	nativeRetryReadResponse(t, response, http.StatusRequestEntityTooLarge, nil)
}

func TestNativeSessionSizePolicyRequestConfigScope(t *testing.T) {
	const limit = 9 << 20
	server := NewServer(nil, nil, ServerConfig{NativeSessionMaxBytes: limit})
	for _, target := range []string{
		"/api/v1/sessions/one/native?namespace=default",
		"http://example.com/API/V1/SESSIONS/one/NATIVE?namespace=default",
		"/api/v1/sessions/one/native#ignored-fragment",
	} {
		header := &fasthttp.RequestHeader{}
		header.SetMethod(http.MethodPost)
		header.SetRequestURI(target)
		configured := server.app.Server().HeaderReceived(header)
		require.Equal(t, harnessv2.NativeSessionJSONLimit(limit), configured.MaxRequestBodySize, target)
		require.Equal(t, 30*time.Second, configured.ReadTimeout, target)
		require.Equal(t, harnessv2.NativeSessionJSONLimit(harnessv2.DefaultMaxNativeSessionBytes), requestBodyConfig(header).MaxRequestBodySize)
		header.SetMethod(http.MethodGet)
		require.Equal(t, requestBodyConfig(header), server.app.Server().HeaderReceived(header))
	}
	for _, target := range []string{
		"/api/v1/tasks", "/api/v1/sessions/one/native/extra", "/api/v1/sessions/one/events",
		"/api/v1/gateways/default/one/events", "/internal/v2/acp/authorize",
	} {
		header := &fasthttp.RequestHeader{}
		header.SetMethod(http.MethodPost)
		header.SetRequestURI(target)
		require.Equal(t, requestBodyConfig(header), server.app.Server().HeaderReceived(header), target)
	}
}
