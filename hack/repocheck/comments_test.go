/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package repocheck

import (
	"testing"

	"github.com/orka-agents/orka/hack/commentlint/scan"
)

func TestCommentCountsMatchBaseline(t *testing.T) {
	root := repoRoot(t)
	trackedFiles(t, root)
	comments, err := scan.Inventory(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := scan.LoadBaseline(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range scan.Compare(scan.Counts(comments), baseline) {
		t.Error(v)
	}
}
