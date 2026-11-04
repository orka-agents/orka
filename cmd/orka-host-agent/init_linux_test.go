/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"path/filepath"
	"testing"
)

func TestInitAdoptsAndReapsOrphans(t *testing.T) {
	// The orphan records its parent after it has been orphaned, then exits.
	// The command exits 100 when init did not adopt it, and otherwise with
	// the number of zombies init has left behind.
	parent := filepath.Join(t.TempDir(), "parent")
	cmd, output := initProcess(t, `
( sh -c 'sleep 0.1; set -- $(cat /proc/$$/stat); echo "$4" > "$0"' "$1" & )
sleep 0.6
[ "$(cat "$1")" = "$PPID" ] || exit 100
n=0
for s in /proc/[0-9]*/stat; do
	set -- $(cat "$s" 2>/dev/null)
	if [ "$3" = Z ] && [ "$4" = "$PPID" ]; then n=$((n+1)); fi
done
exit "$n"`, parent)
	if got := exitCode(t, cmd.Run()); got != 0 {
		t.Fatalf("init exited %d (100: the orphan was not adopted; otherwise zombies left); output: %s", got, output)
	}
}
