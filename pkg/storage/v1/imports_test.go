package v1_test

import (
	"errors"
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The contract, its provider suite and its wire library must be usable by a
// separately built provider, so no package in the storage module may depend
// on Orka's internal packages or anything outside the module.
func TestStorageModuleImportsOnlyItselfAndItsDependencies(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	moduleDir := filepath.Dir(filepath.Dir(file))
	const modulePath = "github.com/orka-agents/orka/pkg/storage"
	packages := 0
	err := filepath.WalkDir(moduleDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		if entry.Name() == "testdata" || entry.Name() == "proto" {
			return filepath.SkipDir
		}
		pkg, err := build.ImportDir(path, 0)
		if err != nil {
			if _, ok := errors.AsType[*build.NoGoError](err); ok {
				return nil
			}
			return err
		}
		packages++
		imports := append(append(append([]string(nil), pkg.Imports...), pkg.TestImports...), pkg.XTestImports...)
		for _, imported := range imports {
			if strings.HasPrefix(imported, "github.com/orka-agents/") && !strings.HasPrefix(imported, modulePath+"/") {
				t.Errorf("%s imports %s", path, imported)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if packages < 5 {
		t.Fatalf("found only %d packages under %s", packages, moduleDir)
	}
}

// An adapter that imports the contract inherits the module's requirements,
// so they stay limited to gRPC, protobuf, the google.rpc status types and the
// assertion library the shared suite uses.
func TestStorageModuleRequiresOnlyTheWireDependencies(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	content, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(file)), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"github.com/stretchr/testify":               true,
		"google.golang.org/grpc":                    true,
		"google.golang.org/protobuf":                true,
		"google.golang.org/genproto/googleapis/rpc": true,
	}
	inRequire := false
	for line := range strings.SplitSeq(string(content), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inRequire = true
			continue
		case line == ")":
			inRequire = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimPrefix(line, "require ")
		case !inRequire:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasSuffix(line, "// indirect") {
			continue
		}
		if !allowed[fields[0]] {
			t.Errorf("the storage module requires %s directly", fields[0])
		}
	}
}
