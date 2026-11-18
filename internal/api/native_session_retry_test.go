package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/orka-agents/orka/internal/codexstate"
	"github.com/orka-agents/orka/internal/store"
	storetest "github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
)

type nativeRetryLostResponse struct {
	status int
	body   []byte
	err    error
}

type nativeRetryResponseLoss struct {
	method   string
	path     string
	status   int
	attempts atomic.Int32
	dropped  atomic.Bool
	response chan nativeRetryLostResponse
}

func (loss *nativeRetryResponseLoss) handle(c fiber.Ctx) error {
	matches := c.Method() == loss.method && c.Path() == loss.path
	if matches {
		loss.attempts.Add(1)
	}
	if err := c.Next(); err != nil {
		return err
	}
	if !matches || c.Response().StatusCode() != loss.status || !loss.dropped.CompareAndSwap(false, true) {
		return nil
	}
	// The real handler has returned success, so SQLite has committed. Keep
	// the would-be response as an oracle, then close TCP before sending it.
	response := nativeRetryLostResponse{status: c.Response().StatusCode(), body: bytes.Clone(c.Response().Body())}
	response.err = c.Drop()
	loss.response <- response
	return response.err
}

func nativeRetryTakeLostResponse(t *testing.T, loss *nativeRetryResponseLoss) nativeRetryLostResponse {
	t.Helper()
	select {
	case response := <-loss.response:
		require.NoError(t, response.err)
		require.Equal(t, loss.status, response.status)
		require.EqualValues(t, 1, loss.attempts.Load(), "the HTTP client must not silently retry the dropped response")
		return response
	case <-time.After(5 * time.Second):
		t.Fatal("the successful response was not dropped")
		return nativeRetryLostResponse{}
	}
}

func nativeRetryStartHTTP(t *testing.T, app *fiber.App) (string, *http.Client) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		require.NoError(t, app.ShutdownWithTimeout(5*time.Second))
		select {
		case err := <-serveErrors:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			_ = listener.Close()
			t.Error("native retry HTTP server did not stop")
		}
	})
	return "http://" + listener.Addr().String(), client
}

func nativeRetryHTTPRequest(t *testing.T, client *http.Client, method, url string, value any) (*http.Response, error) {
	t.Helper()
	var body io.Reader
	if value != nil {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, url, body)
	require.NoError(t, err)
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return client.Do(request)
}

func nativeRetryReadResponse(t *testing.T, response *http.Response, status int, target any) {
	t.Helper()
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, status, response.StatusCode)
	if target != nil {
		require.NoError(t, json.NewDecoder(response.Body).Decode(target))
	}
}

func TestNativeSessionRetryHTTPImportLostResponseAfterCommit(t *testing.T) {
	s := nativeAPIStore(t)
	const path = "/sessions/lost-import/native"
	loss := &nativeRetryResponseLoss{method: http.MethodPost, path: path, status: http.StatusCreated, response: make(chan nativeRetryLostResponse, 1)}
	base, client := nativeRetryStartHTTP(t, nativeAPIApp(t, HandlersConfig{SessionStore: s}, loss.handle))
	snapshot := storetest.NativeSessionSnapshot(t, "offline imported history")
	request := nativeSessionImportRequest{OperationID: "lost-import-operation", Data: snapshot.Data}

	response, err := nativeRetryHTTPRequest(t, client, http.MethodPost, base+path, request)
	require.Error(t, err, "the first committed import must be ambiguous to the client")
	require.Nil(t, response)
	lost := nativeRetryTakeLostResponse(t, loss)
	var receipt store.NativeSessionImportReceipt
	require.NoError(t, json.Unmarshal(lost.body, &receipt))
	require.Equal(t, "default", receipt.Namespace)
	require.Equal(t, "lost-import", receipt.SessionName)
	require.Equal(t, request.OperationID, receipt.OperationID)
	require.Equal(t, snapshot.DataDigest, receipt.DataDigest)
	require.Equal(t, store.NativeSessionImportDigest(receipt.Namespace, receipt.SessionName, snapshot.DataDigest), receipt.RequestDigest)
	require.Equal(t, snapshot.ProviderSessionID, receipt.ProviderSessionID)
	providerID, err := uuid.Parse(receipt.ProviderSessionID)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, providerID)
	require.Equal(t, providerID.String(), receipt.ProviderSessionID)
	require.False(t, receipt.CreatedAt.IsZero())

	// Prove the first request committed without relying on the retry to create it.
	sessions, err := s.ListSessions(t.Context(), receipt.Namespace)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, receipt.SessionName, sessions[0].Name)
	require.Equal(t, "task", sessions[0].SessionType)
	require.Zero(t, sessions[0].MessageCount)
	require.Empty(t, sessions[0].ActiveTask)
	uid, err := s.GetSessionCleanupIdentity(t.Context(), receipt.Namespace, receipt.SessionName)
	require.NoError(t, err)
	require.Empty(t, uid, "an import reserves a name, not a runtime or SessionControl UID")
	staged, err := s.GetNativeSession(t.Context(), receipt.Namespace, receipt.SessionName, "")
	require.NoError(t, err)
	require.Equal(t, snapshot.Data, staged.Snapshot.Data)
	require.Equal(t, request.OperationID, staged.SourceOperationID)
	require.Equal(t, receipt.CreatedAt, staged.CreatedAt)
	require.Empty(t, staged.Snapshot.RuntimeSessionUID)
	require.Zero(t, staged.RuntimeSessionGeneration)

	// A delayed import retry must not unbind an owner or overwrite a later checkpoint.
	const ownerUID = "11111111-1111-4111-8111-111111111111"
	require.NoError(t, s.BindSessionCleanupIdentity(t.Context(), receipt.Namespace, receipt.SessionName, ownerUID))
	continued := storetest.NativeSessionSnapshot(t, "offline checkpoint after import")
	require.NoError(t, s.SaveNativeSession(t.Context(), store.NativeSessionRecord{
		Namespace: receipt.Namespace, SessionName: receipt.SessionName, SessionUID: ownerUID,
		Snapshot: continued, SourceOperationID: "capture-after-lost-import", RuntimeSessionGeneration: 1,
	}))
	checkpoint, err := s.GetNativeSession(t.Context(), receipt.Namespace, receipt.SessionName, ownerUID)
	require.NoError(t, err)

	response, err = nativeRetryHTTPRequest(t, client, http.MethodPost, base+path, request)
	require.NoError(t, err)
	var retry store.NativeSessionImportReceipt
	nativeRetryReadResponse(t, response, http.StatusCreated, &retry)
	require.Equal(t, receipt, retry)
	changed := request
	changed.Data = storetest.NativeSessionSnapshot(t, "different import with the same operation ID").Data
	response, err = nativeRetryHTTPRequest(t, client, http.MethodPost, base+path, changed)
	require.NoError(t, err)
	nativeRetryReadResponse(t, response, http.StatusConflict, nil)
	changed = request
	changed.OperationID = "competing-import-operation"
	response, err = nativeRetryHTTPRequest(t, client, http.MethodPost, base+path, changed)
	require.NoError(t, err)
	nativeRetryReadResponse(t, response, http.StatusConflict, nil)
	require.EqualValues(t, 4, loss.attempts.Load())

	after, err := s.GetNativeSession(t.Context(), receipt.Namespace, receipt.SessionName, ownerUID)
	require.NoError(t, err)
	require.Equal(t, checkpoint, after)
	_, err = s.GetNativeSession(t.Context(), receipt.Namespace, receipt.SessionName, "")
	require.ErrorIs(t, err, store.ErrConflict)
	afterSessions, err := s.ListSessions(t.Context(), receipt.Namespace)
	require.NoError(t, err)
	require.Equal(t, sessions, afterSessions, "retries must not allocate another Session or mutate its transcript")
	response, err = nativeRetryHTTPRequest(t, client, http.MethodGet, base+path, nil)
	require.NoError(t, err)
	var exported nativeSessionExportResponse
	nativeRetryReadResponse(t, response, http.StatusOK, &exported)
	require.Equal(t, continued.Data, exported.Data)
	require.Equal(t, continued.DataDigest, exported.DataDigest)
	require.Equal(t, continued.ProviderSessionID, exported.ProviderSessionID)
}

func TestNativeSessionRetryHTTPExportLostResponse(t *testing.T) {
	for _, bound := range []bool{false, true} {
		name := "staged-import"
		if bound {
			name = "bound-checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			s := nativeAPIStore(t)
			const path = "/sessions/lost-export/native"
			loss := &nativeRetryResponseLoss{method: http.MethodGet, path: path, status: http.StatusOK, response: make(chan nativeRetryLostResponse, 1)}
			base, client := nativeRetryStartHTTP(t, nativeAPIApp(t, HandlersConfig{SessionStore: s}, loss.handle))
			snapshot := storetest.NativeSessionSnapshot(t, "offline export history")
			request := nativeSessionImportRequest{OperationID: "export-seed-operation", Data: snapshot.Data}
			response, err := nativeRetryHTTPRequest(t, client, http.MethodPost, base+path, request)
			require.NoError(t, err)
			var receipt store.NativeSessionImportReceipt
			nativeRetryReadResponse(t, response, http.StatusCreated, &receipt)
			uid := ""
			if bound {
				uid = "22222222-2222-4222-8222-222222222222"
				require.NoError(t, s.BindSessionCleanupIdentity(t.Context(), "default", "lost-export", uid))
				snapshot = storetest.NativeSessionSnapshot(t, "offline captured export history")
				require.NoError(t, s.SaveNativeSession(t.Context(), store.NativeSessionRecord{
					Namespace: "default", SessionName: "lost-export", SessionUID: uid,
					Snapshot: snapshot, SourceOperationID: "export-capture-operation", RuntimeSessionGeneration: 1,
				}))
			}
			before, err := s.GetNativeSession(t.Context(), "default", "lost-export", uid)
			require.NoError(t, err)
			sessionsBefore, err := s.ListSessions(t.Context(), "default")
			require.NoError(t, err)

			response, err = nativeRetryHTTPRequest(t, client, http.MethodGet, base+path, nil)
			require.Error(t, err)
			require.Nil(t, response)
			lost := nativeRetryTakeLostResponse(t, loss)
			var first nativeSessionExportResponse
			require.NoError(t, json.Unmarshal(lost.body, &first))
			require.Equal(t, snapshot.Data, first.Data)
			require.Equal(t, snapshot.DataDigest, first.DataDigest)
			require.Equal(t, snapshot.ProviderSessionID, first.ProviderSessionID)
			for range 2 {
				response, err = nativeRetryHTTPRequest(t, client, http.MethodGet, base+path, nil)
				require.NoError(t, err)
				var retry nativeSessionExportResponse
				nativeRetryReadResponse(t, response, http.StatusOK, &retry)
				require.Equal(t, first, retry)
			}
			require.EqualValues(t, 3, loss.attempts.Load())
			response, err = nativeRetryHTTPRequest(t, client, http.MethodPost, base+path, request)
			require.NoError(t, err)
			var retryReceipt store.NativeSessionImportReceipt
			nativeRetryReadResponse(t, response, http.StatusCreated, &retryReceipt)
			require.Equal(t, receipt, retryReceipt)
			after, err := s.GetNativeSession(t.Context(), "default", "lost-export", uid)
			require.NoError(t, err)
			require.Equal(t, before, after)
			sessionsAfter, err := s.ListSessions(t.Context(), "default")
			require.NoError(t, err)
			require.Equal(t, sessionsBefore, sessionsAfter)
		})
	}
}

func TestNativeSessionRetryHTTPDeletedNameAndUIDIsolation(t *testing.T) {
	for _, coordinated := range []bool{false, true} {
		name := "direct-delete"
		if coordinated {
			name = "coordinated-cleanup"
		}
		t.Run(name, func(t *testing.T) {
			s := nativeAPIStore(t)
			app := nativeAPIApp(t, HandlersConfig{SessionStore: s}, nil)
			h := NewHandlers(HandlersConfig{SessionStore: s})
			app.Delete("/sessions/:id", h.DeleteSession)
			base, client := nativeRetryStartHTTP(t, app)
			const sessionName = "deleted-import"
			path := base + "/sessions/" + sessionName + "/native"
			snapshot := storetest.NativeSessionSnapshot(t, "offline deleted history")
			request := nativeSessionImportRequest{OperationID: "retained-import-operation", Data: snapshot.Data}
			response, err := nativeRetryHTTPRequest(t, client, http.MethodPost, path, request)
			require.NoError(t, err)
			var receipt store.NativeSessionImportReceipt
			nativeRetryReadResponse(t, response, http.StatusCreated, &receipt)
			const oldUID = "33333333-3333-4333-8333-333333333333"
			const replacementUID = "44444444-4444-4444-8444-444444444444"
			require.NoError(t, s.BindSessionCleanupIdentity(t.Context(), "default", sessionName, oldUID))
			require.ErrorIs(t, s.BindSessionCleanupIdentity(t.Context(), "default", sessionName, replacementUID), store.ErrConflict)
			_, err = s.GetNativeSession(t.Context(), "default", sessionName, replacementUID)
			require.ErrorIs(t, err, store.ErrConflict)
			original, err := s.GetNativeSession(t.Context(), "default", sessionName, oldUID)
			require.NoError(t, err)
			require.Equal(t, snapshot.Data, original.Snapshot.Data)

			if coordinated {
				intent := store.SessionCleanupIntent{
					Namespace: "default", SessionName: sessionName, SessionUID: oldUID,
					OperationID: "delete-native-operation", OperationDigest: store.CanonicalBytesDigest([]byte("delete-native-operation")),
					PreparedAt: time.Now().UTC(),
				}
				_, err = s.PrepareSessionCleanup(t.Context(), intent)
				require.NoError(t, err)
				response, err = nativeRetryHTTPRequest(t, client, http.MethodPost, path, request)
				require.NoError(t, err)
				nativeRetryReadResponse(t, response, http.StatusConflict, nil)
				response, err = nativeRetryHTTPRequest(t, client, http.MethodGet, path, nil)
				require.NoError(t, err)
				nativeRetryReadResponse(t, response, http.StatusConflict, nil)
				require.NoError(t, s.CompleteSessionCleanup(t.Context(), store.CompleteSessionCleanupRequest{
					Namespace: intent.Namespace, SessionName: intent.SessionName, OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
				}))
			}
			response, err = nativeRetryHTTPRequest(t, client, http.MethodDelete, base+"/sessions/"+sessionName, nil)
			require.NoError(t, err)
			nativeRetryReadResponse(t, response, http.StatusNoContent, nil)
			for _, uid := range []string{"", oldUID, replacementUID} {
				_, err = s.GetNativeSession(t.Context(), "default", sessionName, uid)
				require.ErrorIs(t, err, store.ErrNotFound)
			}

			// Native deletion permanently reserves the name. Recreating a new
			// owner through the public store is rejected, rather than bypassing
			// the tombstone with SQL and testing an unreachable ownership state.
			require.ErrorIs(t, s.CreateSession(t.Context(), &store.SessionRecord{
				Namespace: "default", Name: sessionName, SessionType: "task",
			}), store.ErrConflict)
			require.ErrorIs(t, s.BindSessionCleanupIdentity(t.Context(), "default", sessionName, replacementUID), store.ErrNotFound)
			changed := request
			changed.Data = storetest.NativeSessionSnapshot(t, "changed retained import").Data
			fresh := request
			fresh.OperationID = "new-owner-import-operation"
			for _, retry := range []nativeSessionImportRequest{request, changed, fresh} {
				response, err = nativeRetryHTTPRequest(t, client, http.MethodPost, path, retry)
				require.NoError(t, err)
				nativeRetryReadResponse(t, response, http.StatusConflict, nil)
			}
			for _, url := range []string{path, base + "/sessions/" + sessionName} {
				response, err = nativeRetryHTTPRequest(t, client, http.MethodGet, url, nil)
				require.NoError(t, err)
				nativeRetryReadResponse(t, response, http.StatusNotFound, nil)
			}
			sessions, err := s.ListSessions(t.Context(), "default")
			require.NoError(t, err)
			require.Empty(t, sessions, "retained imports must not resurrect deleted Session ownership")
		})
	}
}

func TestNativeSessionRetryHTTPAcceptsExactEncodedBundleLimit(t *testing.T) {
	s := nativeAPIStore(t)
	base, client := nativeRetryStartHTTP(t, nativeAPIApp(t, HandlersConfig{SessionStore: s}, nil))
	snapshot := storetest.NativeSessionSnapshot(t, "offline transport boundary")
	// JSON whitespace preserves the verified manifest and rollout while
	// making the actual encoded bundle exactly the advertised transport cap.
	data := append(bytes.Clone(snapshot.Data), bytes.Repeat([]byte{' '}, maxNativeSessionBundleBytes-len(snapshot.Data))...)
	require.Len(t, data, maxNativeSessionBundleBytes)
	summary, err := codexstate.Inspect(t.Context(), data)
	require.NoError(t, err, "the boundary fixture must be a valid bundle, not malformed oversized input")
	request := nativeSessionImportRequest{OperationID: "exact-limit-operation", Data: data}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	require.Greater(t, len(body), maxNativeSessionBundleBytes, "JSON base64 transport adds bytes beyond the bundle cap")
	require.Less(t, len(body), 2*maxNativeSessionBundleBytes)
	response, err := nativeRetryHTTPRequest(t, client, http.MethodPost, base+"/sessions/exact-limit/native", request)
	require.NoError(t, err)
	var receipt store.NativeSessionImportReceipt
	nativeRetryReadResponse(t, response, http.StatusCreated, &receipt)
	require.Equal(t, summary.DataDigest, receipt.DataDigest)
	require.Equal(t, snapshot.ProviderSessionID, receipt.ProviderSessionID)
	response, err = nativeRetryHTTPRequest(t, client, http.MethodGet, base+"/sessions/exact-limit/native", nil)
	require.NoError(t, err)
	var exported nativeSessionExportResponse
	nativeRetryReadResponse(t, response, http.StatusOK, &exported)
	require.Equal(t, data, exported.Data)
	require.Equal(t, summary.DataDigest, exported.DataDigest)
	require.Equal(t, snapshot.ProviderSessionID, exported.ProviderSessionID)
}
