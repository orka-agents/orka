package supervisor

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestSupervisorV2ProbeAuthenticationBoundary(t *testing.T) {
	server, cfg, _ := newTestServer(t, "immediate")
	validCapability, err := harnessv2.SignStatusCapability(cfg.CapabilitySecret, harnessv2.NewStatusCapabilityClaims(time.Now().UTC().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	wrongCapability, err := harnessv2.SignStatusCapability([]byte(strings.Repeat("q", 32)), harnessv2.NewStatusCapabilityClaims(time.Now().UTC().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		path          string
		authorization string
		capability    string
		wantStatus    int
	}{
		{name: "health is public", path: harnessv2.HealthPath, wantStatus: http.StatusOK},
		{name: "capabilities are public", path: harnessv2.CapabilitiesPath, wantStatus: http.StatusOK},
		{name: "status rejects missing authorization", path: harnessv2.StatusPath, wantStatus: http.StatusUnauthorized},
		{name: "status rejects wrong bearer", path: harnessv2.StatusPath, authorization: "Bearer " + strings.Repeat("w", 32), wantStatus: http.StatusUnauthorized},
		{name: "status rejects bare token", path: harnessv2.StatusPath, authorization: cfg.ControllerBearerToken, wantStatus: http.StatusUnauthorized},
		{name: "status rejects wrong scheme", path: harnessv2.StatusPath, authorization: "Basic " + cfg.ControllerBearerToken, wantStatus: http.StatusUnauthorized},
		{name: "status rejects bearer without capability", path: harnessv2.StatusPath, authorization: "Bearer " + cfg.ControllerBearerToken, wantStatus: http.StatusForbidden},
		{name: "status rejects bearer with wrong-key capability", path: harnessv2.StatusPath, authorization: "Bearer " + cfg.ControllerBearerToken, capability: wrongCapability, wantStatus: http.StatusForbidden},
		{name: "status accepts controller bearer with capability", path: harnessv2.StatusPath, authorization: "Bearer " + cfg.ControllerBearerToken, capability: validCapability, wantStatus: http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			if test.capability != "" {
				request.Header.Set(OperationCapabilityHeader, test.capability)
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("GET %s status = %d, want %d", test.path, response.Code, test.wantStatus)
			}
			if test.wantStatus != http.StatusUnauthorized {
				return
			}
			var apiError harnessv2.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &apiError); err != nil {
				t.Fatalf("decode auth rejection: %v", err)
			}
			if apiError.Code != harnessv2.ErrorCodeUnauthenticated {
				t.Fatalf("auth rejection code = %q, want %q", apiError.Code, harnessv2.ErrorCodeUnauthenticated)
			}
		})
	}
}

// unreadableBody proves the mutation body is never read on the
// unauthenticated path: any Read fails the test.
type unreadableBody struct{ t *testing.T }

func (b *unreadableBody) Read([]byte) (int, error) {
	b.t.Fatal("unauthenticated mutation must be rejected before the request body is read")
	return 0, io.EOF
}

func TestSupervisorMutationsAuthenticateBeforeReadingBodies(t *testing.T) {
	server, _, _ := newTestServer(t, "immediate")
	paths := []struct {
		method string
		path   string
	}{
		{method: http.MethodPut, path: harnessv2.DrainPath},
		{method: http.MethodPut, path: "/v2/runtime-sessions/session-a"},
		{method: http.MethodPut, path: "/v2/runtime-sessions/session-a/publication-finalization"},
		{method: http.MethodDelete, path: "/v2/runtime-sessions/session-a"},
		{method: http.MethodPut, path: "/v2/runtime-sessions/session-a/prompts/prompt-a"},
		{method: http.MethodPut, path: "/v2/runtime-sessions/session-a/prompts/prompt-a/lease"},
		{method: http.MethodPut, path: "/v2/runtime-sessions/session-a/prompts/prompt-a/permissions/request-a"},
		{method: http.MethodPut, path: "/v2/runtime-sessions/session-a/prompts/prompt-a/cancel"},
		{method: http.MethodPut, path: "/v2/runtime-sessions/session-a/workspace-deltas/delta-a"},
	}
	for _, test := range paths {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, &unreadableBody{t: t})
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s status = %d, want %d", test.method, test.path, response.Code, http.StatusUnauthorized)
			}
		})
	}
}
