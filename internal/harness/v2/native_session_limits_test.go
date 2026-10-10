package v2

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNativeSessionLimitPolicy(t *testing.T) {
	limit, err := NormalizeNativeSessionMaxBytes(0)
	require.NoError(t, err)
	require.Equal(t, 8<<20, limit)
	for _, n := range []int{1, LegacyMaxNativeSessionBytes, DefaultMaxNativeSessionBytes, MaxNativeSessionBytes} {
		limit, err = NormalizeNativeSessionMaxBytes(n)
		require.NoError(t, err)
		require.Equal(t, n, limit)
	}
	for _, n := range []int{-1, MaxNativeSessionBytes + 1} {
		_, err = NormalizeNativeSessionMaxBytes(n)
		require.Error(t, err)
		limits := DefaultProtocolLimits()
		limits.MaxNativeSessionBytes = n
		require.Error(t, limits.Validate())
	}
	legacy := DefaultProtocolLimits()
	legacy.MaxNativeSessionBytes = 0
	require.NoError(t, legacy.Validate())
	require.Equal(t, LegacyMaxNativeSessionBytes, legacy.EffectiveMaxNativeSessionBytes())
	require.Equal(t, DefaultMaxNativeSessionBytes, DefaultProtocolLimits().EffectiveMaxNativeSessionBytes())
}

func TestLargeNativeRestoreDigestAndTransport(t *testing.T) {
	now := time.Now().UTC()
	request := clientTestCreateSessionRequest(t, now, "large-native-create")
	snapshot := testNativeSnapshot(request.Metadata.Fence)
	snapshot.Data = bytes.Repeat([]byte("x"), 2<<20)
	snapshot.DataDigest = testSHA256(string(snapshot.Data))
	request.NativeRestore = &NativeSessionRestore{Snapshot: snapshot}
	digest, err := CanonicalRequestDigest(request)
	require.NoError(t, err)
	pointerDigest, err := CanonicalRequestDigest(&request)
	require.NoError(t, err)
	require.Equal(t, digest, pointerDigest)
	request.Metadata.RequestDigest = digest
	require.NoError(t, request.ValidateAt(now))
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	require.Greater(t, len(raw), MaxCanonicalJSONBytes)
	_, err = CanonicalRequestDigestJSON(raw)
	require.Error(t, err, "ordinary raw request digests remain bounded")
	_, err = CanonicalJSON(raw)
	require.Error(t, err)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var received CreateRuntimeSessionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		require.NoError(t, received.ValidateAt(time.Now().UTC()))
		require.Equal(t, snapshot.Data, received.NativeRestore.Snapshot.Data)
		w.Header().Set("Content-Type", "application/json")
		response := clientTestCreateSessionResponse(request, now, RequestClassificationFresh)
		response.Session.ProviderSessionID = snapshot.ProviderSessionID
		response.Session.NativeRestoration = &NativeSessionRestoration{DataDigest: snapshot.DataDigest, ProviderSessionID: snapshot.ProviderSessionID, Loaded: true}
		require.NoError(t, json.NewEncoder(w).Encode(response))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithControllerBearerToken(clientTestBearer), WithOperationCapabilitySecret(clientTestCapabilitySecret))
	require.NoError(t, err)
	_, err = client.CreateRuntimeSession(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	legacy := DefaultProtocolLimits()
	legacy.MaxNativeSessionBytes = 0
	require.NoError(t, WithProtocolLimits(legacy)(client))
	_, err = client.CreateRuntimeSession(t.Context(), request)
	require.Error(t, err)
	require.Equal(t, 1, calls, "oversized restore must fail before sending to old runtime")
}

func TestLargeNativeCaptureResponseTransport(t *testing.T) {
	now := time.Now().UTC()
	create := clientTestCreateSessionRequest(t, now, "create")
	request := CaptureNativeSessionRequest{Protocol: ProtocolVersion, Metadata: clientTestMetadata(t, now, "capture", false)}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	snapshot := testNativeSnapshot(request.Metadata.Fence)
	snapshot.Data = bytes.Repeat([]byte("x"), 2<<20)
	snapshot.DataDigest = testSHA256(string(snapshot.Data))
	response := CaptureNativeSessionResponse{Protocol: ProtocolVersion, Classification: Classification{Class: RequestClassificationFresh}, Session: clientTestCreateSessionResponse(create, now, RequestClassificationFresh).Session, Snapshot: snapshot}
	response.Session.ProviderSessionID = snapshot.ProviderSessionID
	response.Session.State = RuntimeSessionStatePoisoned
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(response))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithControllerBearerToken(clientTestBearer), WithOperationCapabilitySecret(clientTestCapabilitySecret))
	require.NoError(t, err)
	result, err := client.CaptureNativeSession(t.Context(), create.RuntimeSessionID, request)
	require.NoError(t, err)
	require.Equal(t, snapshot.Data, result.Snapshot.Data)
	limits := DefaultProtocolLimits()
	limits.MaxNativeSessionBytes = 1 << 20
	require.NoError(t, WithProtocolLimits(limits)(client))
	_, err = client.CaptureNativeSession(t.Context(), create.RuntimeSessionID, request)
	require.Error(t, err)
}
