// Command wiregen writes the orka.storage.v1 Go-to-wire mapping.
//
// It reads the Go contract (pkg/storage/v1) with go/types and the wire
// descriptors from the generated storagepb registry, checks that the two
// match completely, and writes:
//
//   - zz_generated.mapping.go: conversions between every contract record and
//     its wire message;
//   - zz_generated.service.go: the method table, a Client method and a server
//     handler for every RPC;
//   - wiretest/zz_generated.overlay.go: the test overlay that serves the
//     families with RPCs over the wire.
//
// Generation fails, writing nothing, when a contract method has no RPC or an
// RPC no method, when a parameter, result or record field (including a field
// tagged json:"-") has no wire counterpart or the reverse, when a pair of
// types is not in the mapping table, or when revision annotations are
// inconsistent. The wire has no "ignore this field" option.
//
// Run it with go generate ./v1/wire from the storage module.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

func main() {
	contractDir := flag.String("contract", "..", "directory of the Go contract package")
	wireDir := flag.String("wire", ".", "directory of the wire package")
	wiretestDir := flag.String("wiretest", "wiretest", "directory of the wiretest package")
	flag.Parse()
	if err := run(*contractDir, *wireDir, *wiretestDir); err != nil {
		fmt.Fprintln(os.Stderr, "wiregen:", err)
		os.Exit(1)
	}
}

func run(contractDir, wireDir, wiretestDir string) error {
	contract, err := loadContract(contractDir)
	if err != nil {
		return err
	}
	revision, err := loadProtocolRevision(filepath.Join(wireDir, "protocol.go"))
	if err != nil {
		return err
	}
	m, err := buildModel(contract, wireFiles(), revision)
	if err != nil {
		return err
	}
	outputs, err := render(m)
	if err != nil {
		return err
	}
	for name, content := range outputs {
		dir := wireDir
		if rest, ok := strings.CutPrefix(name, "wiretest/"); ok {
			dir, name = wiretestDir, rest
		}
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// wireFiles returns the orka.storage.v1 descriptors linked into this binary.
func wireFiles() []protoreflect.FileDescriptor {
	_ = storagepb.File_orka_storage_v1_options_proto
	var files []protoreflect.FileDescriptor
	protoregistry.GlobalFiles.RangeFilesByPackage(wirePackage, func(file protoreflect.FileDescriptor) bool {
		files = append(files, file)
		return true
	})
	return files
}
