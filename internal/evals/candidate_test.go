package evals

import (
	"strings"
	"testing"
)

func TestParseCandidate(t *testing.T) {
	limits := CandidateLimits{Models: []string{"openai/gpt-4.1"}, CurrentSystemPrompt: "Review carefully.", CurrentModel: "openai/gpt-4o-mini"}
	const valid = `{"schemaVersion":"orka.improvement.candidate.v1","hypothesis":"The reviewer misses unit bugs.",` +
		`"agentPatch":{"systemPrompt":"Review carefully.\nCheck units.","model":"openai/gpt-4.1"}}`
	candidate, err := ParseCandidate([]byte("Here is my answer:\n```json\n"+valid+"\n```\n"), limits)
	if err != nil {
		t.Fatalf("ParseCandidate() error = %v", err)
	}
	if candidate.Hypothesis != "The reviewer misses unit bugs." || candidate.AgentPatch.Model != "openai/gpt-4.1" ||
		!strings.Contains(candidate.AgentPatch.SystemPrompt, "Check units.") {
		t.Fatalf("candidate = %+v", candidate)
	}

	// A model left as it is drops out of the patch.
	same, err := ParseCandidate([]byte(strings.Replace(valid, `"openai/gpt-4.1"`, `"openai/gpt-4o-mini"`, 1)),
		CandidateLimits{Models: []string{"openai/gpt-4o-mini"}, CurrentSystemPrompt: "Review carefully.", CurrentModel: "openai/gpt-4o-mini"})
	if err != nil || same.AgentPatch.Model != "" || same.AgentPatch.SystemPrompt == "" {
		t.Fatalf("unchanged model: %+v, %v", same, err)
	}

	for name, test := range map[string]struct {
		answer string
		limits CandidateLimits
		want   string
	}{
		"no JSON":           {answer: "I would change the prompt.", want: "no JSON object"},
		"wrong schema":      {answer: strings.Replace(valid, "candidate.v1", "candidate.v2", 1), want: "schemaVersion"},
		"unknown field":     {answer: strings.Replace(valid, `"hypothesis":`, `"tools":["Bash"],"hypothesis":`, 1), want: "unknown field"},
		"other Agent field": {answer: strings.Replace(valid, `"model":`, `"runtime":"codex","model":`, 1), want: "unknown field"},
		"blank hypothesis":  {answer: strings.Replace(valid, "The reviewer misses unit bugs.", " ", 1), want: "hypothesis is required"},
		"hidden characters": {answer: strings.Replace(valid, "Check units.", "Check\u202e units.", 1), want: "formatting character"},
		"long prompt": {answer: strings.Replace(valid, "Check units.", strings.Repeat("x", MaxCandidateSystemPromptBytes), 1),
			want: "at most"},
		"model not allowed": {answer: strings.Replace(valid, "openai/gpt-4.1", "openai/o3-pro", 1), want: "not one of the allowed models"},
		"prompt changes only": {answer: valid, limits: CandidateLimits{CurrentSystemPrompt: "x"},
			want: "prompt changes only"},
		"no change": {answer: `{"schemaVersion":"orka.improvement.candidate.v1","hypothesis":"h","agentPatch":{"systemPrompt":"Review carefully."}}`,
			want: "changes neither"},
		"empty patch": {answer: `{"schemaVersion":"orka.improvement.candidate.v1","hypothesis":"h","agentPatch":{}}`,
			want: "changes neither"},
	} {
		t.Run(name, func(t *testing.T) {
			use := limits
			if test.limits.CurrentSystemPrompt != "" || test.limits.Models != nil {
				use = test.limits
			}
			if _, err := ParseCandidate([]byte(test.answer), use); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseCandidate() error = %v, want %q", err, test.want)
			}
		})
	}
}
