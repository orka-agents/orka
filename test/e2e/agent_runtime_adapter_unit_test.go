//go:build e2e

package e2e

import (
	"slices"
	"testing"
)

func TestParseRuntimeSessionAdaptersSelectsSessionGroupLeaders(t *testing.T) {
	const output = "Owner: 61001\n" +
		// The supervisor.
		"Name:\torka-acp-runtim\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t1\nNgid:\t0\nPid:\t1\nPPid:\t0\n" +
		"Uid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\nNSpgid:\t1\nNSsid:\t1\n" +
		// The session's adapter.
		"Name:\tnode\nPid:\t42\nPPid:\t1\nUid:\t61001\t61001\t61001\t61001\nNSpgid:\t42\n" +
		// A descendant in the adapter's process group.
		"Name:\tclaude\nPid:\t57\nPPid:\t42\nUid:\t61001\t61001\t61001\t61001\nNSpgid:\t42\n" +
		// Another session's adapter.
		"Name:\topencode\nPid:\t80\nPPid:\t1\nUid:\t61002\t61002\t61002\t61002\nNSpgid:\t80\n" +
		// The exec'd shell reading /proc.
		"Name:\tsh\nPid:\t99\nPPid:\t0\nUid:\t0\t0\t0\t0\nNSpgid:\t99\n"
	owner, adapters, err := parseRuntimeSessionAdapters(output)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "61001" || !slices.Equal(adapters, []int{42}) {
		t.Fatalf("owner %q adapters %v, want 61001 [42]", owner, adapters)
	}

	owner, adapters, err = parseRuntimeSessionAdapters(
		"Owner: 61001\nName:\tsh\nPid:\t99\nPPid:\t0\nUid:\t0\nNSpgid:\t99\n")
	if err != nil || owner != "61001" || len(adapters) != 0 {
		t.Fatalf("owner %q adapters %v err %v, want an exited adapter to leave none", owner, adapters, err)
	}
}
