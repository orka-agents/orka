package main

import "testing"

func TestTaskPostP0EventCommandsRegistered(t *testing.T) {
	cmd := newTaskCmd()
	want := map[string]bool{
		"events": false, "follow": false, "trace": false, "approvals": false,
		"approve": false, "decline": false, "fork": false,
	}
	for _, sub := range cmd.Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("missing task subcommand %q", name)
		}
	}
}

func TestSessionForkAndEventCommandsRegistered(t *testing.T) {
	cmd := newSessionCmd()
	want := map[string]bool{"events": false, "follow": false, "fork": false}
	for _, sub := range cmd.Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("missing session subcommand %q", name)
		}
	}
}
