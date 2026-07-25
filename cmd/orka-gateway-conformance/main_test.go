package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/conformance"
	"github.com/orka-agents/orka/internal/gateway/protocol"
)

func TestWriteResultRedactsConfiguredValueBeforeJSONOutput(t *testing.T) {
	const sentinel = "redaction-sentinel-value"
	result := conformance.CheckResult{
		Message: "Authorization: Bearer " + sentinel,
		Capabilities: &protocol.CapabilitiesResponse{
			ProtocolVersion: protocol.Version,
			AdapterName:     "adapter-" + sentinel,
			AdapterVersion:  sentinel,
		},
	}
	var output bytes.Buffer
	if err := writeResult(&output, result, sentinel); err != nil {
		t.Fatalf("writeResult() error = %v", err)
	}
	if strings.Contains(output.String(), sentinel) {
		t.Fatalf("writeResult() leaked credential: %s", output.String())
	}
	if !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("writeResult() output = %s, want redaction marker", output.String())
	}
}
