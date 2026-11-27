package supervisor

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/acp"
)

const (
	copilotDisabledToolsDiagnosticPrefix       = "Info: Disabled tools: "
	copilotUnknownExcludedToolDiagnosticPrefix = "Info: Unknown tool name in the tool excludedlist: "
)

func copilotStartupDiagnostic(excludedTools []string) func(string) (string, bool) {
	excluded := make(map[string]struct{}, len(excludedTools))
	for _, name := range excludedTools {
		excluded[name] = struct{}{}
	}
	return func(text string) (string, bool) {
		if list, ok := strings.CutPrefix(text, copilotDisabledToolsDiagnosticPrefix); ok {
			return copilotDisabledToolsSummary(list, excluded)
		}
		if quoted, ok := strings.CutPrefix(text, copilotUnknownExcludedToolDiagnosticPrefix); ok {
			name, err := strconv.Unquote(quoted)
			if err != nil {
				return "", false
			}
			if _, excludedName := excluded[name]; !excludedName {
				return "", false
			}
			return "unknown excluded tool " + strconv.Quote(name), true
		}
		return "", false
	}
}

func copilotDisabledToolsSummary(list string, excluded map[string]struct{}) (string, bool) {
	var known []string
	others := 0
	for entry := range strings.SplitSeq(list, ",") {
		name := strings.TrimSpace(entry)
		if !copilotToolIdentifier(name) {
			return "", false
		}
		if _, ok := excluded[name]; ok {
			known = append(known, name)
		} else {
			others++
		}
	}
	if len(known) == 0 {
		return "", false
	}
	summary := "disabled tools: " + strings.Join(known, ", ")
	if others > 0 {
		summary += " (+" + strconv.Itoa(others) + " not in the session exclusion list)"
	}
	return summary, true
}

func copilotToolIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func withholdAgentDiagnostic(state *sessionState, prompt *promptState, event acp.PromptEvent) bool {
	filter := state.agentDiagnosticFilter
	if filter == nil || filter.Startup == nil {
		return false
	}
	text, ok := assistantMessageText(event)
	if !ok || text == "" {
		return false
	}
	summary, recognized := filter.Startup(text)
	if !recognized {
		return false
	}
	promptID := prompt.request.Metadata.PromptID
	if state.providerProxy.modelOutputPossibleAt(string(promptID), promptEventReceivedAt(event)) {
		return false
	}
	slog.Info(
		"ACP provider CLI startup diagnostic withheld from the agent message stream",
		"runtimeSession", state.id, "promptID", promptID, "sequence", event.Sequence, "diagnostic", summary,
	)
	return true
}

func promptEventReceivedAt(event acp.PromptEvent) time.Time {
	if !event.ReceivedAt.IsZero() {
		return event.ReceivedAt
	}
	return event.Timestamp
}
