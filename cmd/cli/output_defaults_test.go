/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// structuredDefaultAllowlist names the commands that may keep a non-table
// default for --output, with the reason. Everything else that shows an
// object or list must print a readable view by default; scripts pass
// -o json. Add to this list only with a reason a reviewer would accept.
var structuredDefaultAllowlist = map[string]string{
	// The repository monitor command family prints nested workflow
	// documents (plans, readiness reports, command receipts, mutations)
	// whose shape has no short readable projection yet. YAML keeps them
	// scannable. Giving each of them a field list is follow-up work; new
	// monitor commands should not be added here without one.
	"orka monitor actions get":              "monitor workflow document (YAML)",
	"orka monitor commands create":          "monitor workflow document (YAML)",
	"orka monitor commands get":             "monitor workflow document (YAML)",
	"orka monitor doctor":                   "monitor workflow document (YAML)",
	"orka monitor implementations get":      "monitor workflow document (YAML)",
	"orka monitor issue approve-plan":       "monitor workflow document (YAML)",
	"orka monitor issue decompose":          "monitor workflow document (YAML)",
	"orka monitor issue implement":          "monitor workflow document (YAML)",
	"orka monitor issue implementation get": "monitor workflow document (YAML)",
	"orka monitor issue patch preview":      "monitor workflow document (YAML)",
	"orka monitor issue plan":               "monitor workflow document (YAML)",
	"orka monitor issue research":           "monitor workflow document (YAML)",
	"orka monitor issue resume":             "monitor workflow document (YAML)",
	"orka monitor issue status":             "monitor workflow document (YAML)",
	"orka monitor issue stop":               "monitor workflow document (YAML)",
	"orka monitor issue triage":             "monitor workflow document (YAML)",
	"orka monitor issues get":               "monitor workflow document (YAML)",
	"orka monitor mutations get":            "monitor workflow document (YAML)",
	"orka monitor pr automerge":             "monitor workflow document (YAML)",
	"orka monitor pr fix":                   "monitor workflow document (YAML)",
	"orka monitor pr fix-ci":                "monitor workflow document (YAML)",
	"orka monitor pr ready readiness":       "monitor workflow document (YAML)",
	"orka monitor pr resume":                "monitor workflow document (YAML)",
	"orka monitor pr review":                "monitor workflow document (YAML)",
	"orka monitor pr status":                "monitor workflow document (YAML)",
	"orka monitor pr stop":                  "monitor workflow document (YAML)",
	"orka monitor pr update-branch":         "monitor workflow document (YAML)",
	"orka monitor trigger-labels validate":  "monitor workflow document (YAML)",
	"orka monitor watch":                    "monitor workflow document (YAML)",
	"orka monitor work-actions get":         "monitor workflow document (YAML)",
}

// TestNoCommandDefaultsToStructuredOutput walks the whole command tree and
// fails when a command's --output flag defaults to json or yaml without an
// allowlisted reason, so the next new `get` command cannot quietly default
// to JSON again.
func TestNoCommandDefaultsToStructuredOutput(t *testing.T) {
	var offenders []string
	var walk func(cmd *cobra.Command, path string)
	walk = func(cmd *cobra.Command, path string) {
		if flag := cmd.Flags().Lookup("output"); flag != nil && flag.Usage == "Output format: table, json, yaml" {
			def := strings.ToLower(flag.DefValue)
			if def != outputTable {
				if _, ok := structuredDefaultAllowlist[path]; !ok {
					offenders = append(offenders, path+" (default "+def+")")
				}
			}
		}
		for _, sub := range cmd.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(newRootCmd(), "orka")
	if len(offenders) > 0 {
		t.Fatalf("commands default to structured output without an allowlist entry:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestStructuredDefaultAllowlistIsCurrent fails when an allowlist entry no
// longer names a command with a structured default, so the list cannot
// rot.
func TestStructuredDefaultAllowlistIsCurrent(t *testing.T) {
	found := map[string]bool{}
	var walk func(cmd *cobra.Command, path string)
	walk = func(cmd *cobra.Command, path string) {
		if flag := cmd.Flags().Lookup("output"); flag != nil && strings.ToLower(flag.DefValue) != outputTable {
			found[path] = true
		}
		for _, sub := range cmd.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(newRootCmd(), "orka")
	for path := range structuredDefaultAllowlist {
		if !found[path] {
			t.Errorf("allowlist entry %q no longer defaults to structured output; remove it", path)
		}
	}
}
