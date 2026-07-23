package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.test
spec:
  group: example.test
  names:
    kind: Widget
    plural: widgets
  scope: Namespaced
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
`

func TestExtractCRDKind(t *testing.T) {
	kind, err := extractCRDKind(testCRD)
	if err != nil {
		t.Fatalf("extractCRDKind() error = %v", err)
	}
	if kind != "Widget" {
		t.Fatalf("extractCRDKind() = %q, want Widget", kind)
	}
}

func TestObjectSetWritesOnlyCRDs(t *testing.T) {
	destination := t.TempDir()
	oldOutput := *outputDir
	*outputDir = destination
	t.Cleanup(func() { *outputDir = oldOutput })

	set := objectSet{byKind: map[string][]string{
		generatedKind: {testCRD},
		"Deployment": {`apiVersion: apps/v1
kind: Deployment
metadata:
  name: ignored
`},
	}}
	if err := set.write(); err != nil {
		t.Fatalf("write() error = %v", err)
	}

	crdPath := filepath.Join(destination, "crds", "widget-customresourcedefinition.yaml")
	contents, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read generated CRD: %v", err)
	}
	if !strings.Contains(string(contents), "kind: CustomResourceDefinition") {
		t.Fatalf("generated CRD does not contain the CRD kind")
	}
	if _, err := os.Stat(filepath.Join(destination, "templates")); !os.IsNotExist(err) {
		t.Fatalf("non-CRD objects unexpectedly produced a templates directory: %v", err)
	}
}

func TestObjectSetRejectsDuplicateCRDFilenames(t *testing.T) {
	destination := t.TempDir()
	oldOutput := *outputDir
	*outputDir = destination
	t.Cleanup(func() { *outputDir = oldOutput })

	set := objectSet{byKind: map[string][]string{generatedKind: {testCRD, testCRD}}}
	if err := set.write(); err == nil || !strings.Contains(err.Error(), "duplicate generated output filename") {
		t.Fatalf("write() error = %v, want duplicate filename error", err)
	}
}

func TestCopyStaticFilesRejectsSymlinks(t *testing.T) {
	static := t.TempDir()
	destination := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(static, "linked")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	oldOutput := *outputDir
	*outputDir = destination
	t.Cleanup(func() { *outputDir = oldOutput })

	if err := copyStaticFiles(static); err == nil || !strings.Contains(err.Error(), "unsupported static chart entry") {
		t.Fatalf("copyStaticFiles() error = %v, want unsupported entry error", err)
	}
}
