package v1alpha1

import "testing"

func TestRepositoryMonitorHumanFeedbackAdmission(t *testing.T) {
	admit := admitSpec(t, "../../config/crd/bases/core.orka.ai_repositorymonitors.yaml", "spec")
	base := func() map[string]any {
		return map[string]any{
			"repoURL": "https://github.com/example-org/example-repo",
			"repair": map[string]any{
				"enabled":       true,
				"humanFeedback": map[string]any{"enabled": true},
			},
		}
	}
	feedback := func(s map[string]any) map[string]any {
		return s["repair"].(map[string]any)["humanFeedback"].(map[string]any)
	}
	for _, tc := range []struct {
		name   string
		patch  func(map[string]any)
		reject bool
	}{
		{name: "enabled"},
		{name: "all fields", patch: func(s map[string]any) {
			f := feedback(s)
			f["minPermission"], f["botLogins"], f["changesRequestedReviews"] = "maintain", []any{"copilot-pull-request-reviewer", "renovate[bot]"}, false
			f["maxCommentsPerRepair"], f["maxAttemptsPerComment"] = int64(50), int64(10)
		}},
		{name: "repair disabled", patch: func(s map[string]any) { s["repair"].(map[string]any)["enabled"] = false }, reject: true},
		{name: "disabled feedback without repair", patch: func(s map[string]any) {
			s["repair"] = map[string]any{"humanFeedback": map[string]any{"enabled": false}}
		}},
		{name: "read permission", patch: func(s map[string]any) { feedback(s)["minPermission"] = "read" }, reject: true},
		{name: "bad bot login", patch: func(s map[string]any) { feedback(s)["botLogins"] = []any{"bad login"} }, reject: true},
		{name: "too many comments", patch: func(s map[string]any) { feedback(s)["maxCommentsPerRepair"] = int64(51) }, reject: true},
		{name: "zero attempts", patch: func(s map[string]any) { feedback(s)["maxAttemptsPerComment"] = int64(0) }, reject: true},
		{name: "too many attempts", patch: func(s map[string]any) { feedback(s)["maxAttemptsPerComment"] = int64(11) }, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := base()
			if tc.patch != nil {
				tc.patch(spec)
			}
			if admitted := admit(spec); admitted == tc.reject {
				t.Fatalf("admitted = %t, want %t", admitted, !tc.reject)
			}
		})
	}
}
