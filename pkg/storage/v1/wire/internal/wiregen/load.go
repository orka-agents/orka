package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	contractPath = "github.com/orka-agents/orka/pkg/storage/v1"
	wirePackage  = "orka.storage.v1"
)

// loadContract type-checks the Go contract package from its source. The
// contract imports only the standard library, which the source importer
// resolves, so no module loader is needed.
func loadContract(dir string) (*types.Package, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go contract sources in %s", dir)
	}
	config := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	return config.Check(contractPath, fset, files, nil)
}

// loadProtocolRevision reads the wire package's ProtocolRevision constant
// from source, so the generator never compiles the package it writes.
func loadProtocolRevision(path string) (int, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return 0, err
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			for index, name := range value.Names {
				if name.Name != "ProtocolRevision" || index >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.INT {
					return 0, errors.New("ProtocolRevision must be an integer literal")
				}
				return strconv.Atoi(literal.Value)
			}
		}
	}
	return 0, fmt.Errorf("%s declares no ProtocolRevision", path)
}
