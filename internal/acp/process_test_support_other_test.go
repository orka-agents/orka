//go:build !linux

package acp

import (
	"os"
	"path/filepath"
	"testing"
)

func testExecHelperCommand(_ *testing.T) string { return "" }
func cleanupTestExecHelper()                    {}

func testAdapterCommand(t *testing.T) string {
	t.Helper()
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return command
}
