package v1_test

import (
	"go/build"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestContractPackagesDoNotImportOrkaInternals(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	contractDir := filepath.Dir(file)
	for _, dir := range []string{contractDir, filepath.Join(contractDir, "storagetest")} {
		pkg, err := build.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("read package %s: %v", dir, err)
		}
		imports := append(append(append([]string(nil), pkg.Imports...), pkg.TestImports...), pkg.XTestImports...)
		for _, path := range imports {
			if strings.HasPrefix(path, "github.com/orka-agents/orka/") &&
				!strings.HasPrefix(path, "github.com/orka-agents/orka/pkg/") {
				t.Errorf("%s imports %s", pkg.ImportPath, path)
			}
		}
	}
}
