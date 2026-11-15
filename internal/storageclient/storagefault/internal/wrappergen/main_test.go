package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestGeneratedWrappersAreCurrent fails when wrappers_gen.go differs from the
// generator's output, for example after a provider interface gained a method.
// The package builds without the generated file, so the test still runs while
// the file is out of date.
func TestGeneratedWrappersAreCurrent(t *testing.T) {
	want, err := generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", "wrappers_gen.go"))
	if err != nil {
		t.Fatalf("read wrappers_gen.go: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("internal/storageclient/storagefault/wrappers_gen.go is out of date; run go generate ./internal/storageclient/storagefault")
	}
}
