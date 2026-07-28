package main

import (
	"bytes"
	"net/http/httptest"
	"testing"

	workspaceagent "github.com/orka-agents/orka/pkg/workspaceagent"
)

func FuzzWorkspaceAgentRequestDecoding(f *testing.F) {
	f.Add([]byte(`{"protocolVersion":"workspace.orka.ai/v1","operationID":"op","command":["true"]}`))
	f.Add([]byte(`{}`))
	f.Add(bytes.Repeat([]byte("x"), 1025))

	f.Fuzz(func(t *testing.T, payload []byte) {
		server := newWorkspaceAgentServer()
		server.maxRequestBytes = 1024
		request := httptest.NewRequest("POST", workspaceagent.ExecPath, bytes.NewReader(payload))
		var decoded execRequest
		_ = server.decodeJSON(request, &decoded)
	})
}

func FuzzWorkspaceAgentPathValidation(f *testing.F) {
	f.Add("/tmp/file")
	f.Add("../../etc/passwd")
	f.Add("workspace/file")
	f.Add("")

	f.Fuzz(func(t *testing.T, value string) {
		_, _ = safePath(value)
	})
}

func FuzzWorkspaceAgentIdentifiersAndDigests(f *testing.F) {
	f.Add("operation-1", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	f.Add("../operation", "not-a-digest")

	f.Fuzz(func(t *testing.T, operationID, digest string) {
		_ = validOperationID(operationID)
		_, _ = parseSHA256Digest(digest)
	})
}
