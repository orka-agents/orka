package remediation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSourceSelectionRejectsAmbiguousOrUnstructuredOutput(t *testing.T) {
	for _, raw := range []string{
		`{"files":["README"]}`,
		`{"files":["README","src/main.go"]}`,
	} {
		if _, err := DecodeSourceSelection(raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{
		`{}`, `null`, `[]`,
		`{"files":[]}`,
		`{"files":[""]}`,
		`{"files":["README"],"files":["other"]}`,
		`{"files":["README"],"Files":["other"]}`,
		`{"files":["README"],"publish":true}`,
		`{"files":["README"]} {}`,
		"```json\n{\"files\":[\"README\"]}\n```",
	} {
		if _, err := DecodeSourceSelection(raw); err == nil {
			t.Fatal("malformed proposal was accepted")
		}
	}
}

func TestCheckProposalFilesAreFrozenEntrypoints(t *testing.T) {
	base := `{"scope":["normal and problematic input"],"gaps":[],"requirements":[],"files":[{"path":"check.sh","content":"#!/bin/sh\nexit 125\n","executable":true}],"checks":[{"id":"repro","kind":"reproduction","command":["/checks/check.sh"],"healthy":{"exitCode":0,"stdout":"healthy"},"failure":{"exitCode":1,"stdout":"broken"},"timeoutSeconds":10}],"services":[]}`
	if _, err := DecodeCheckProposal(base); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []struct{ before, after string }{
		{`"path":"check.sh"`, `"path":"../check.sh"`},
		{`"path":"check.sh"`, `"path":"/check.sh"`},
		{`"executable":true`, `"executable":false`},
		{`"/checks/check.sh"`, `"/src/check.sh"`},
		{`"/checks/check.sh"`, `"/checks/../src/check.sh"`},
		{`"/checks/check.sh"`, `"/bin/sh"`},
		{`"scope":["normal and problematic input"]`, `"scope":[]`},
	} {
		_, err := DecodeCheckProposal(strings.Replace(base, replacement.before, replacement.after, 1))
		if err == nil {
			t.Fatalf("unsafe proposal accepted: %s", replacement.before)
		}
	}
}

func TestPatchProposalCannotReplaceDiffWithClaim(t *testing.T) {
	proposal := PatchProposal{
		Summary: "Reject invalid quantities",
		Patch:   "diff --git a/main.c b/main.c\n--- a/main.c\n+++ b/main.c\n",
	}
	raw, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePatchProposal(string(raw)); err == nil {
		t.Fatal("missing declared change was accepted")
	}
	if _, err := DecodePatchProposal(`{"summary":"fixed","patch":"Everything is now fixed.","declaredChanges":[{"kind":"source","description":"fix"}],"limitations":[]}`); err == nil {
		t.Fatal("model's written claim was accepted as a patch")
	}
	if _, err := DecodePatchProposal(`{"summary":"fix","patch":"diff --git a/x b/x\n--- a/x\n+++ b/x\n","declaredChanges":[{"kind":"source","description":"fix"}],"limitations":[]}`); err != nil {
		t.Fatal(err)
	}
}

func TestProposalBoundsAndSensitiveErrors(t *testing.T) {
	for _, raw := range []string{
		strings.Repeat(" ", maxProposalBytes+1),
		"{\"problem\":\"\xff\"}",
		`{"unexpected":"sensitive-example-do-not-repeat"}`,
	} {
		_, err := DecodeSourceSelection(raw)
		if err == nil || strings.Contains(err.Error(), "sensitive-example-do-not-repeat") {
			t.Fatal("invalid proposal accepted or private payload leaked")
		}
	}
}
