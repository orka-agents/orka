package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestNativeSessionProductionServerChunkedImport(t *testing.T) {
	s := nativeAPIStore(t)
	kube := kubefake.NewClientset()
	kube.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})
	const token = "native-transport-test-only-token"
	server := NewServer(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:default:native-transport"}), nil, ServerConfig{SessionStore: s, Clientset: kube, WatchNamespace: "default"})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { require.NoError(t, server.app.Shutdown()); require.NoError(t, <-serveErrors) })
	snapshot := storetest.NativeSessionSnapshot(t, "synthetic native transport history")
	body, err := json.Marshal(nativeSessionImportRequest{OperationID: "chunked-production", Data: snapshot.Data})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/api/v1/sessions/chunked-production/native?namespace=default", io.NopCloser(bytes.NewReader(body)))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	require.NoError(t, response.Body.Close())
	checkpoint, err := s.GetNativeSession(t.Context(), "default", "chunked-production", "")
	require.NoError(t, err)
	require.Equal(t, snapshot.Data, checkpoint.Snapshot.Data)

	// An adjacent ordinary API write must still require a declared length.
	request, err = http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/api/v1/tasks", io.NopCloser(strings.NewReader(`{}`)))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusLengthRequired, response.StatusCode)
	require.NoError(t, response.Body.Close())

	// A peer that announces a huge chunk but never completes it is rejected at
	// the native route's limit, without waiting for the rest of that chunk.
	connection, err := net.Dial("tcp4", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	require.NoError(t, connection.SetDeadline(time.Now().Add(5*time.Second)))
	const oversized = 2*maxNativeSessionBundleBytes + 1
	_, err = fmt.Fprintf(connection, "POST /api/v1/sessions/chunked-too-large/native HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n", token, 2*oversized)
	require.NoError(t, err)
	_, err = connection.Write(bytes.Repeat([]byte{' '}, oversized))
	require.NoError(t, err)
	response, err = http.ReadResponse(bufio.NewReader(connection), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
	require.NoError(t, response.Body.Close())
	_, err = s.GetSession(t.Context(), "default", "chunked-too-large")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestNativeSessionImportPathTransportScope(t *testing.T) {
	for _, path := range []string{"/api/v1/sessions/one/native", "/API/V1/SESSIONS/one/NATIVE/"} {
		require.True(t, isNativeSessionImportPath(path), path)
	}
	for _, path := range []string{"/sessions/one/native", "/api/v1/sessions/one/native/extra", "/api/v1/sessions//native", "/api/v1/tasks/one/native", "/api/v1/sessions/one/events"} {
		require.False(t, isNativeSessionImportPath(path), path)
	}
}

func TestNativeSessionImportRequestConfig(t *testing.T) {
	for _, target := range []string{
		"/api/v1/sessions/one/native?namespace=default",
		"http://example.com/API/V1/SESSIONS/one/NATIVE?namespace=default",
		"/api/v1/sessions/one/native#ignored-fragment",
	} {
		header := &fasthttp.RequestHeader{}
		header.SetMethod(http.MethodPost)
		header.SetRequestURI(target)
		config := requestBodyConfig(header)
		require.Equal(t, 2*maxNativeSessionBundleBytes, config.MaxRequestBodySize, target)
		require.Equal(t, 30*time.Second, config.ReadTimeout, target)
		header.SetMethod(http.MethodGet)
		config = requestBodyConfig(header)
		require.Zero(t, config.MaxRequestBodySize)
		require.Zero(t, config.ReadTimeout)
	}
}
