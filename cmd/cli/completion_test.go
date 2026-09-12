/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestCLIFlagCompletion(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantLines []string
		directive string
	}{
		{
			name:      "output formats",
			args:      []string{"task", "list", "--output", ""},
			wantLines: []string{"table", "json", "yaml"},
			directive: ":4",
		},
		{
			name:      "output formats shorthand",
			args:      []string{"task", "list", "-o", ""},
			wantLines: []string{"table", "json", "yaml"},
			directive: ":4",
		},
		{
			name:      "output prefix filters",
			args:      []string{"task", "list", "--output", "j"},
			wantLines: []string{"json"},
			directive: ":4",
		},
		{
			name:      "task types",
			args:      []string{"task", "create", "--type", ""},
			wantLines: []string{"ai", "container", "agent"},
			directive: ":4",
		},
		{
			name: "task statuses",
			args: []string{"task", "list", "--status", ""},
			wantLines: []string{
				string(corev1alpha1.TaskPhasePending),
				string(corev1alpha1.TaskPhaseRunning),
				string(corev1alpha1.TaskPhaseFinalizing),
				string(corev1alpha1.TaskPhaseSucceeded),
				string(corev1alpha1.TaskPhaseFailed),
				string(corev1alpha1.TaskPhaseScheduled),
				string(corev1alpha1.TaskPhaseCancelled),
			},
			directive: ":4",
		},
		{
			name:      "task status prefix filters",
			args:      []string{"task", "list", "--status", "S"},
			wantLines: []string{"Succeeded", "Scheduled"},
			directive: ":4",
		},
		{
			name:      "unmatched prefix yields no suggestions",
			args:      []string{"task", "list", "--status", "zz"},
			wantLines: nil,
			directive: ":4",
		},
		{
			name:      "download output keeps filename completion",
			args:      []string{"task", "download", "--output", ""},
			wantLines: nil,
			directive: ":0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)

			args := append([]string{"__complete"}, tt.args...)
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error: %v", err)
			}

			var gotLines []string
			for line := range strings.SplitSeq(strings.TrimRight(out.String(), "\n"), "\n") {
				if strings.TrimSpace(line) != "" {
					gotLines = append(gotLines, line)
				}
			}
			if len(gotLines) == 0 {
				t.Fatalf("no output from __complete")
			}
			if got := gotLines[len(gotLines)-1]; got != tt.directive {
				t.Fatalf("directive = %q, want %q (output: %q)", got, tt.directive, out.String())
			}
			gotSuggestions := gotLines[:len(gotLines)-1]
			if len(tt.wantLines) == 0 {
				if len(gotSuggestions) != 0 {
					t.Fatalf("suggestions = %v, want none", gotSuggestions)
				}
				return
			}
			if strings.Join(gotSuggestions, "\n") != strings.Join(tt.wantLines, "\n") {
				t.Fatalf("suggestions = %v, want %v", gotSuggestions, tt.wantLines)
			}
		})
	}
}
