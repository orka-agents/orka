package supervisor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
)

func TestCopilotNativeShellPermissionIdentity(t *testing.T) {
	// This is the permission envelope emitted by the pinned CLI during live
	// execution. Its human-readable title is not a tool name.
	const observed = `{"toolCallId":"call_shell","title":"Run requested read-only command","kind":"execute","status":"pending","rawInput":{"command":"uname -s","commands":["uname -s"]}}`
	for _, test := range []struct {
		name, provider, toolCall, want string
	}{
		{name: "observed Copilot command", provider: providerKindCopilot, toolCall: observed, want: providerToolBash},
		{name: "other provider", provider: providerKindCodex, toolCall: observed},
		{name: "explicit name wins", provider: providerKindCopilot, toolCall: `{"name":"unreviewed_tool","kind":"execute","rawInput":{"command":"uname -s","commands":["uname -s"]}}`, want: "unreviewed_tool"},
		{name: "execute kind alone", provider: providerKindCopilot, toolCall: `{"kind":"execute","title":"Bash"}`},
		{name: "different kind", provider: providerKindCopilot, toolCall: `{"kind":"other","rawInput":{"command":"uname -s","commands":["uname -s"]}}`},
		{name: "inconsistent commands", provider: providerKindCopilot, toolCall: `{"kind":"execute","rawInput":{"command":"uname -s","commands":["another command"]}}`},
		{name: "multiple commands", provider: providerKindCopilot, toolCall: `{"kind":"execute","rawInput":{"command":"uname -s","commands":["uname -s","another command"]}}`},
		{name: "empty command", provider: providerKindCopilot, toolCall: `{"kind":"execute","rawInput":{"command":" ","commands":[" "]}}`},
		{name: "malformed command list", provider: providerKindCopilot, toolCall: `{"kind":"execute","rawInput":{"command":"uname -s","commands":"uname -s"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			permission, err := mapPermission(&acp.PermissionRequestEvent{
				RequestID: "permission-1",
				Request:   acp.RequestPermissionRequest{ToolCall: json.RawMessage(test.toolCall)},
			}, time.Now(), time.Minute, test.provider)
			if err != nil {
				t.Fatal(err)
			}
			if permission.ToolName != test.want {
				t.Fatalf("permission tool = %q, want %q", permission.ToolName, test.want)
			}
		})
	}
}
