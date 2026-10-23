package storageclient

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"testing"

	"github.com/orka-agents/orka/internal/storageclient/internal/envelopegen"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The decorators are generated from the provider interfaces and the method
// catalog, so a write added to either cannot ship without an envelope.
func TestEnvelopeDecoratorsAreGenerated(t *testing.T) {
	want, err := envelopegen.Generate("storageclient")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile("envelope_gen.go")
	if err != nil {
		t.Fatalf("read envelope_gen.go: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("envelope_gen.go is stale; run make generate")
	}
}

// Each decorator declares exactly the write methods of its provider; reads
// and administrative methods are promoted from the provider unchanged.
func TestDecoratorsDeclareExactlyTheWrites(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "envelope_gen.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse envelope_gen.go: %v", err)
	}
	declared := map[string]map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		receiver, ok := fn.Recv.List[0].Type.(*ast.Ident)
		if !ok {
			t.Fatalf("%s has an unexpected receiver", fn.Name.Name)
		}
		if declared[receiver.Name] == nil {
			declared[receiver.Name] = map[string]bool{}
		}
		declared[receiver.Name][fn.Name.Name] = true
	}
	for _, tc := range []struct {
		decorator string
		iface     reflect.Type
		contract  string
	}{
		{"envelopedState", reflect.TypeFor[storagev1.StateProvider](), storagev1.StateContract},
		{"envelopedArtifacts", reflect.TypeFor[storagev1.ArtifactProvider](), storagev1.ArtifactsContract},
	} {
		writes := 0
		for method := range tc.iface.Methods() {
			name := method.Name
			info, ok := storagev1.LookupMethod(name)
			if !ok {
				t.Fatalf("%s is missing from the method catalog", name)
			}
			isWrite := info.Kind == storagev1.MethodWrite
			if isWrite {
				writes++
				if info.Contract != tc.contract {
					t.Errorf("%s is a %s write served by the %s provider", name, info.Contract, tc.contract)
				}
			}
			if isWrite != declared[tc.decorator][name] {
				t.Errorf("%s.%s: write=%v, declared by the decorator=%v", tc.decorator, name, isWrite, declared[tc.decorator][name])
			}
		}
		if len(declared[tc.decorator]) != writes {
			t.Errorf("%s declares %d methods for %d writes", tc.decorator, len(declared[tc.decorator]), writes)
		}
	}
}
