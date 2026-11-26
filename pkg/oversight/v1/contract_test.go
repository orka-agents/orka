/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1_test

import (
	"encoding/json"
	"errors"
	"go/build"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
)

// The contract must be usable by a separately built pipeline, so it may not
// depend on Orka's internal packages.
func TestContractDoesNotImportOrkaInternals(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	pkg, err := build.ImportDir(filepath.Dir(file), 0)
	if err != nil {
		t.Fatal(err)
	}
	imports := append(append(append([]string(nil), pkg.Imports...), pkg.TestImports...), pkg.XTestImports...)
	for _, imported := range imports {
		if strings.HasPrefix(imported, "github.com/orka-agents/orka/") &&
			!strings.HasPrefix(imported, "github.com/orka-agents/orka/pkg/") {
			t.Errorf("%s imports %s", pkg.ImportPath, imported)
		}
	}
}

func validDecision() oversightv1.OversightDecision {
	threshold := 0.5
	return oversightv1.OversightDecision{
		ID: "task-a@1", TaskName: "task-a", ActionSeq: 1,
		ObservedAt: time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC),
		Mode:       oversightv1.ModeAudit, Disposition: oversightv1.DispositionAuditFlagged,
		EvidenceLevel: oversightv1.EvidenceLevelObserved,
		Coverage:      oversightv1.ActionCoverage{StreamState: "ok", SensorState: "events"},
		Action: oversightv1.ActionEnvelope{
			ToolName: "code_exec",
			Effects: oversightv1.ObservedEffects{
				Counts:       map[string]int{"exec": 1, "credential_read": 1, "net_connect": 0},
				Binaries:     []oversightv1.CountedValue{{Value: "/usr/bin/python3", Count: 1}},
				Paths:        []oversightv1.CountedValue{{Value: "/var/run/secrets/kubernetes.io/serviceaccount/token", Count: 1}},
				Destinations: []oversightv1.CountedValue{{Value: "10.96.4.7:80", Count: 1}},
			},
			TaintFlags: []string{"credential_read"},
		},
		Scorers: []oversightv1.ScorerEvaluation{{
			ProviderRef: "scorer-a", ModelID: "model-a", Status: "ok",
			Scores: []oversightv1.ConcernScore{
				{QuestionID: "K1", Probability: 0.9, Threshold: &threshold, Triggered: true},
				{QuestionID: "K2", Probability: 0.1, Threshold: &threshold},
			},
		}},
		Advisors: []oversightv1.AdvisorEvaluation{{
			ProviderRef: "advisor-a", ModelID: "model-b", Status: "content_filtered", Concerns: []string{"K10"},
		}},
	}
}

func TestValidateAcceptsAWellFormedDecision(t *testing.T) {
	d := validDecision()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesMalformedDecisions(t *testing.T) {
	for name, mutate := range map[string]func(*oversightv1.OversightDecision){
		"missing id":          func(d *oversightv1.OversightDecision) { d.ID = " " },
		"slash in id":         func(d *oversightv1.OversightDecision) { d.ID = "a/b" },
		"unknown disposition": func(d *oversightv1.OversightDecision) { d.Disposition = "ok" },
		"unknown mode":        func(d *oversightv1.OversightDecision) { d.Mode = "enforce" },
		"missing coverage":    func(d *oversightv1.OversightDecision) { d.Coverage = oversightv1.ActionCoverage{} },
		"probability over 1":  func(d *oversightv1.OversightDecision) { d.Scorers[0].Scores[0].Probability = 1.5 },
		"scorer filtered":     func(d *oversightv1.OversightDecision) { d.Scorers[0].Status = "content_filtered" },
		"attested without evidence": func(d *oversightv1.OversightDecision) {
			d.EvidenceLevel = oversightv1.EvidenceLevelAttested
		},
		"too many binaries": func(d *oversightv1.OversightDecision) {
			d.Action.Effects.Binaries = make([]oversightv1.CountedValue, oversightv1.MaxCountedValues+1)
		},
		"missing observedAt": func(d *oversightv1.OversightDecision) { d.ObservedAt = time.Time{} },
		"long policyRevision": func(d *oversightv1.OversightDecision) {
			d.PolicyRevision = strings.Repeat("p", oversightv1.MaxShortTextBytes+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := validDecision()
			mutate(&d)
			if err := d.Validate(); !errors.Is(err, oversightv1.ErrInvalid) {
				t.Fatalf("Validate() = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestTriggeredConcernsUnionScorersAndAdvisors(t *testing.T) {
	d := validDecision()
	got := strings.Join(d.TriggeredConcerns(), ",")
	if got != "K1,K10" {
		t.Fatalf("TriggeredConcerns() = %s", got)
	}
	if d.MaxProbability() != 0.9 {
		t.Fatalf("MaxProbability() = %v", d.MaxProbability())
	}
}

func TestSignatureNormalizesSpecificsAway(t *testing.T) {
	a := validDecision()
	b := validDecision()
	b.ID, b.TaskName = "task-b@0", "task-b"
	b.Action.Effects.Paths[0].Value = "/var/run/secrets/other/path/token"
	b.Action.Effects.Destinations[0].Value = "10.96.4.200:80"
	b.Action.Effects.Binaries[0].Value = "/usr/local/bin/python3"
	if oversightv1.SignatureHash(a.Signature()) != oversightv1.SignatureHash(b.Signature()) {
		t.Fatalf("signatures differ:\n%v\n%v", a.Signature(), b.Signature())
	}
	features := make([]string, 0, len(a.Signature()))
	for _, f := range a.Signature() {
		features = append(features, f.String())
	}
	want := "bin:python3,concern:K1,concern:K10,dest:10.96.4.*:80,effect:credential_read,effect:exec," +
		"path:/var/run/secrets/*,taint:credential_read,tool:code_exec"
	if strings.Join(features, ",") != want {
		t.Fatalf("Signature() = %s", strings.Join(features, ","))
	}
	c := validDecision()
	c.Action.ToolName = "file_write"
	score, shared := oversightv1.Jaccard(a.Signature(), c.Signature())
	if len(shared) != 8 || score != 8.0/10.0 {
		t.Fatalf("Jaccard = %v over %d shared", score, len(shared))
	}
}

func TestNormalizers(t *testing.T) {
	for in, want := range map[string]string{
		"/etc/localtime": "/etc/*", "/localtime": "/localtime", "rel/./a": "rel/a", "/a/b/c/d/e": "/a/b/c/*",
	} {
		if got := oversightv1.PathPrefix(in); got != want {
			t.Errorf("PathPrefix(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"sink.ns:80": "sink.ns:80", "1.2.3.4": "1.2.3.*", "[::1]:443": "::1:443"} {
		if got := oversightv1.DestinationClass(in); got != want {
			t.Errorf("DestinationClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAttestationRoundTripsAndOmits(t *testing.T) {
	d := validDecision()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "attestation") || strings.Contains(string(raw), "recordedAt") {
		t.Fatalf("unset reserved fields are not omitted: %s", raw)
	}
	verified := true
	d.Attestation = &oversightv1.AttestationEnvelope{Format: "fmt-v1", Verified: &verified}
	d.EvidenceLevel = oversightv1.EvidenceLevelAttested
	raw, _ = json.Marshal(d)
	var back oversightv1.OversightDecision
	if err := json.Unmarshal(raw, &back); err != nil || back.Attestation == nil || !*back.Attestation.Verified {
		t.Fatalf("attestation did not round-trip: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFeedbackValidate(t *testing.T) {
	ok := oversightv1.DecisionFeedback{Label: oversightv1.FeedbackFalseAlarm}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := oversightv1.DecisionFeedback{Label: "approve"}
	if err := bad.Validate(); !errors.Is(err, oversightv1.ErrInvalid) {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestMonitorFailedMeansATierGaveNoUsableAnswer(t *testing.T) {
	type scorers = []oversightv1.ScorerEvaluation
	type advisors = []oversightv1.AdvisorEvaluation
	scorer := func(trust, status string) oversightv1.ScorerEvaluation {
		return oversightv1.ScorerEvaluation{ProviderRef: "s", ModelID: "m", TrustState: trust, Status: status}
	}
	advisor := func(status string) oversightv1.AdvisorEvaluation {
		return oversightv1.AdvisorEvaluation{ProviderRef: "a", ModelID: "m", Status: status}
	}
	for name, tc := range map[string]struct {
		scorers  []oversightv1.ScorerEvaluation
		advisors []oversightv1.AdvisorEvaluation
		failed   bool
	}{
		"no evaluations":                     {nil, nil, false},
		"the only scorer timed out":          {scorers{scorer("", "timeout")}, nil, true},
		"a shadow scorer timed out":          {scorers{scorer("qualified", "ok"), scorer("shadow", "timeout")}, nil, false},
		"the qualified scorer timed out":     {scorers{scorer("qualified", "timeout"), scorer("shadow", "ok")}, nil, true},
		"only shadow scorers, one answers":   {scorers{scorer("shadow", "ok"), scorer("shadow", "error")}, nil, false},
		"one advisor filtered, one answered": {nil, advisors{advisor("content_filtered"), advisor("ok")}, false},
		"every advisor filtered":             {nil, advisors{advisor("content_filtered")}, true},
	} {
		t.Run(name, func(t *testing.T) {
			d := oversightv1.OversightDecision{Scorers: tc.scorers, Advisors: tc.advisors}
			if got := d.MonitorFailed(); got != tc.failed {
				t.Fatalf("MonitorFailed() = %v, want %v", got, tc.failed)
			}
		})
	}
}
