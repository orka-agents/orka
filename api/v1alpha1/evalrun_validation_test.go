package v1alpha1

import (
	"bytes"
	"encoding/json"
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
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "unchanged"},
		{name: "same timeout spelled differently", mutate: func(s map[string]any) { s["trialTimeout"] = "20m0s" }},
		{name: "subject", mutate: func(s map[string]any) {
			s["subject"].(map[string]any)["repositoryMonitorReview"] = map[string]any{"monitorRef": map[string]any{"name": "other"}}
		}, want: true},
		{name: "cases", mutate: func(s map[string]any) { s["cases"].(map[string]any)["ids"] = []any{"a"} }, want: true},
		{name: "trials", mutate: func(s map[string]any) { s["trials"] = int64(6) }, want: true},
		{name: "graders", mutate: func(s map[string]any) { s["graders"] = []any{"verdict"} }, want: true},
		{name: "maxConcurrentTrials", mutate: func(s map[string]any) { s["maxConcurrentTrials"] = int64(3) }, want: true},
		{name: "trialTimeout", mutate: func(s map[string]any) { s["trialTimeout"] = "21m" }, want: true},
		{name: "maxInfraErrorPercent", mutate: func(s map[string]any) { s["maxInfraErrorPercent"] = int64(11) }, want: true},
		{name: "ttlSecondsAfterFinished", mutate: func(s map[string]any) { s["ttlSecondsAfterFinished"] = int64(1) }, want: true},
		{name: "ttlSecondsAfterFinished removed", mutate: func(s map[string]any) { delete(s, "ttlSecondsAfterFinished") }, want: true},
		{name: "trialTimeout removed", mutate: func(s map[string]any) { delete(s, "trialTimeout") }, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := validEvalRunSpec()
			if test.mutate != nil {
				test.mutate(changed)
			}
			got := validateEvalRunSpec(t, changed, validEvalRunSpec())
			if rejected := strings.Contains(got, "EvalRun spec is immutable"); rejected != test.want {
				t.Fatalf("errors = %q, want immutability rejection %v", got, test.want)
			}
		})
	}
}

// TestEvalRunTypedRoundTripKeepsSpec proves a typed client can update an
// EvalRun created from YAML: metav1.Duration writes "20m" back as "20m0s".
func TestEvalRunTypedRoundTripKeepsSpec(t *testing.T) {
	stored := validEvalRunSpec()
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var typed EvalRunSpec
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(typed)
	if err != nil {
		t.Fatal(err)
	}
	var written map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&written); err != nil {
		t.Fatal(err)
	}
	written = normalizeJSONNumbers(written).(map[string]any)
	if written["trialTimeout"] != "20m0s" {
		t.Fatalf("trialTimeout written as %v, want the canonical 20m0s", written["trialTimeout"])
	}
	if got := validateEvalRunSpec(t, written, stored); got != "" {
		t.Fatalf("typed round trip rejected: %s", got)
	}
}

func normalizeJSONNumbers(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			v[key] = normalizeJSONNumbers(item)
		}
	case []any:
		for i, item := range v {
			v[i] = normalizeJSONNumbers(item)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	}
	return value
}

// TestEvalRunImmutabilityCoversEverySpecField keeps the field-by-field
// immutability rule in step with the spec schema.
func TestEvalRunImmutabilityCoversEverySpecField(t *testing.T) {
	spec := loadCRD(t, evalRunCRD).Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	var rule string
	for _, validation := range spec.XValidations {
		if validation.Message == "EvalRun spec is immutable; create a new EvalRun" {
			rule = validation.Rule
		}
	}
	if rule == "" {
		t.Fatal("spec has no immutability rule")
	}
	for name := range spec.Properties {
		if !strings.Contains(rule, "oldSelf."+name) {
			t.Errorf("immutability rule does not compare spec.%s", name)
		}
	}
}
