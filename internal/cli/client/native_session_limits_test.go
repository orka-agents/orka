package client

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/stretchr/testify/require"
)

func TestNativeSessionClientConfiguredLimit(t *testing.T) {
	const limit = 2 << 20
	data := bytes.Repeat([]byte("x"), limit)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			require.NoError(t, json.NewEncoder(w).Encode(NativeSessionExport{Data: data, DataDigest: "fixture", ProviderSessionID: "fixture"}))
			return
		}
		var request struct {
			OperationID string `json:"operationID"`
			Data        []byte `json:"data"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, data, request.Data)
		require.NoError(t, json.NewEncoder(w).Encode(NativeSessionImportReceipt{OperationID: request.OperationID}))
	}))
	defer server.Close()
	client := NewWithNamespace(server.URL, "", "team")
	client.NativeSessionMaxBytes = limit
	exported, err := client.ExportNativeSession(t.Context(), "test")
	require.NoError(t, err)
	require.Equal(t, data, exported.Data)
	_, err = client.ImportNativeSession(t.Context(), "test", "operation", data)
	require.NoError(t, err)
	_, err = client.ImportNativeSession(t.Context(), "test", "operation", append(bytes.Clone(data), 'x'))
	require.Error(t, err)
	require.Equal(t, 2, calls, "oversized import must not be sent")
	client.NativeSessionMaxBytes = limit - 1
	_, err = client.ExportNativeSession(t.Context(), "test")
	require.Error(t, err)
	client.NativeSessionMaxBytes = -1
	_, err = client.ExportNativeSession(t.Context(), "test")
	require.Error(t, err)
	require.Equal(t, 3, calls)
	client.NativeSessionMaxBytes = harnessv2.MaxNativeSessionBytes + 1
	_, err = client.ImportNativeSession(t.Context(), "test", "operation", data)
	require.Error(t, err)
	require.Equal(t, 3, calls)
}
