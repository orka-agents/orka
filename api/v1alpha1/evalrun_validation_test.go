package v1alpha1

import (
	"fmt"
	"strings"
	"testing"

	structurallisttype "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

const evalRunCRD = "core.orka.ai_evalruns.yaml"

func validEvalRunSpec() map[string]any {
	return map[string]any{
		"subject": map[string]any{
			"type":                    "repositoryMonitorReview",
			"repositoryMonitorReview": map[string]any{"monitorRef": map[string]any{"name": "orka-monitor"}},
		},
		"cases": map[string]any{
			"configMapRef": map[string]any{"name": "pr-review-cases", "key": "cases.jsonl"},
		},
		"trials":                  int64(5),
		"maxConcurrentTrials":     int64(2),
		"trialTimeout":            "20m",
		"graders":                 []any{"verdict", "localization", "contract", "honesty"},
		"maxInfraErrorPercent":    int64(10),
		"ttlSecondsAfterFinished": int64(604800),
	}
}

func caseIDs(n int) []any {
	ids := make([]any, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("case-%03d", i)
	}
	return ids
}

// validateEvalRunSpec runs the OpenAPI schema and the CEL rules for a create,
// or for an update when old is set.
func validateEvalRunSpec(t *testing.T, spec, old map[string]any) string {
	t.Helper()
	rootValidator := loadRootValidator(t, evalRunCRD)
	errs := schemavalidation.ValidateCustomResource(nil, map[string]any{"spec": spec}, rootValidator)
	validator, structural := loadSpecCELValidator(t, evalRunCRD)
	var oldSpec any
	if old != nil {
		oldSpec = old
	}
	celErrs, _ := validator.Validate(t.Context(), nil, structural, spec, oldSpec, celconfig.RuntimeCELCostBudget)
	errs = append(errs, celErrs...)
	errs = append(errs, structurallisttype.ValidateListSetsAndMaps(nil, structural, spec)...)
	if len(errs) == 0 {
		return ""
	}
	return errs.ToAggregate().Error()
}

func TestEvalRunSpecAdmission(t *testing.T) {
	requireSpecAtRoot(t, evalRunCRD, validEvalRunSpec())
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{name: "valid"},
		{name: "minimal", mutate: func(s map[string]any) {
			delete(s, "maxConcurrentTrials")
			delete(s, "trialTimeout")
			delete(s, "maxInfraErrorPercent")
			delete(s, "ttlSecondsAfterFinished")
			s["graders"] = []any{"verdict"}
		}},
		{name: "unknown subject type", mutate: func(s map[string]any) {
			s["subject"] = map[string]any{"type": "chatRouting"}
		}, want: "Unsupported value"},
		{name: "subject member missing", mutate: func(s map[string]any) {
			s["subject"] = map[string]any{"type": "repositoryMonitorReview"}
		}, want: "subject.repositoryMonitorReview is required"},
		{name: "monitor ref without name", mutate: func(s map[string]any) {
			s["subject"].(map[string]any)["repositoryMonitorReview"] = map[string]any{"monitorRef": map[string]any{}}
		}, want: "name"},
		{name: "unknown grader", mutate: func(s map[string]any) {
			s["graders"] = []any{"verdict", "judge"}
		}, want: "Unsupported value"},
		{name: "no graders", mutate: func(s map[string]any) { s["graders"] = []any{} }, want: "graders"},
		{name: "duplicate graders", mutate: func(s map[string]any) {
			s["graders"] = []any{"verdict", "verdict"}
		}, want: "Duplicate value"},
		{name: "no case source", mutate: func(s map[string]any) {
			s["cases"] = map[string]any{"ids": []any{"a"}}
		}, want: "cases.configMapRef is required"},
		{name: "case configmap without key", mutate: func(s map[string]any) {
			s["cases"] = map[string]any{"configMapRef": map[string]any{"name": "pr-review-cases"}}
		}, want: "key"},
		{name: "bad case id", mutate: func(s map[string]any) {
			s["cases"].(map[string]any)["ids"] = []any{"-bad"}
		}, want: "ids"},
		{name: "zero trials", mutate: func(s map[string]any) { s["trials"] = int64(0) }, want: "trials"},
		{name: "too many trials", mutate: func(s map[string]any) { s["trials"] = int64(21) }, want: "trials"},
		{name: "zero concurrency", mutate: func(s map[string]any) { s["maxConcurrentTrials"] = int64(0) }, want: "maxConcurrentTrials"},
		{name: "short trial timeout", mutate: func(s map[string]any) { s["trialTimeout"] = "30s" }, want: "trialTimeout must be from 1m to 2h"},
		{name: "long trial timeout", mutate: func(s map[string]any) { s["trialTimeout"] = "3h" }, want: "trialTimeout must be from 1m to 2h"},
		{name: "infra percent over 100", mutate: func(s map[string]any) { s["maxInfraErrorPercent"] = int64(101) }, want: "maxInfraErrorPercent"},
		{name: "negative ttl", mutate: func(s map[string]any) { s["ttlSecondsAfterFinished"] = int64(-1) }, want: "ttlSecondsAfterFinished"},
		{name: "500 case trials", mutate: func(s map[string]any) {
			s["cases"].(map[string]any)["ids"] = caseIDs(25)
			s["trials"] = int64(20)
		}},
		{name: "over 500 case trials", mutate: func(s map[string]any) {
			s["cases"].(map[string]any)["ids"] = caseIDs(26)
			s["trials"] = int64(20)
		}, want: "cases × trials must be at most 500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := validEvalRunSpec()
			if test.mutate != nil {
				test.mutate(spec)
			}
			got := validateEvalRunSpec(t, spec, nil)
			if test.want == "" {
				if got != "" {
					t.Fatalf("valid spec rejected: %s", got)
				}
				return
			}
			if !strings.Contains(got, test.want) {
				t.Fatalf("errors = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEvalRunSpecIsImmutable(t *testing.T) {
	old := validEvalRunSpec()
	if got := validateEvalRunSpec(t, validEvalRunSpec(), old); got != "" {
		t.Fatalf("unchanged spec rejected: %s", got)
	}
	changed := validEvalRunSpec()
	changed["trials"] = int64(6)
	if got := validateEvalRunSpec(t, changed, old); !strings.Contains(got, "EvalRun spec is immutable") {
		t.Fatalf("errors = %q, want immutability", got)
	}
}
