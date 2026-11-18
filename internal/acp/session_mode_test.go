package acp

import (
	"encoding/json"
	"testing"
)

func TestVerifySessionModeRequiresTheReportedCurrentMode(t *testing.T) {
	const required = "orka-external-ask"
	for _, test := range []struct {
		name     string
		modes    string
		required string
		wantErr  bool
	}{
		{name: "no requirement accepts anything", modes: `{"currentModeId":"agent"}`},
		{name: "no requirement accepts no modes"},
		{name: "required mode reported", modes: `{"currentModeId":"orka-external-ask","availableModes":[]}`, required: required},
		{name: "fallback mode", modes: `{"currentModeId":"agent"}`, required: required, wantErr: true},
		{name: "modes omitted", required: required, wantErr: true},
		{name: "malformed modes", modes: `"orka-external-ask"`, required: required, wantErr: true},
		{name: "null modes", modes: `null`, required: required, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var modes json.RawMessage
			if test.modes != "" {
				modes = json.RawMessage(test.modes)
			}
			if err := verifySessionMode(modes, test.required); (err != nil) != test.wantErr {
				t.Fatalf("verifySessionMode() error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}

func TestBuiltInRuntimesSupportNativePermissionAsk(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "copilot", "opencode", " Copilot "} {
		if !BuiltInRuntimeSupportsNativePermissionAsk(provider) {
			t.Fatalf("built-in runtime %q does not support asking", provider)
		}
	}
	for _, provider := range []string{"", "agentkit", "foundry", "ai"} {
		if BuiltInRuntimeSupportsNativePermissionAsk(provider) {
			t.Fatalf("runtime %q unexpectedly supports asking", provider)
		}
	}
}
