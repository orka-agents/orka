package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
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
	m, err := buildModel(contract, storagev1.LookupMethod, wireFiles(), revision)
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

func wireFiles() []protoreflect.FileDescriptor {
	_ = storagepb.File_orka_storage_v1_options_proto
	var files []protoreflect.FileDescriptor
	protoregistry.GlobalFiles.RangeFilesByPackage(wirePackage, func(file protoreflect.FileDescriptor) bool {
		files = append(files, file)
		return true
	})
	return files
}
