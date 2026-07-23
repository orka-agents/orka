// Modified from open-policy-agent/gatekeeper cmd/build/helmify at
// c9b67657102032a460a28e7f3b9c88ec0c193453.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

var (
	outputDir = flag.String(
		"output-dir",
		"manifest_staging/charts/orka",
		"root directory in which to write the generated Helm chart",
	)
	staticDir = flag.String(
		"static-dir",
		"third_party/open-policy-agent/gatekeeper/helmify/static",
		"directory containing static Helm chart inputs",
	)
)

var (
	kindRegex = regexp.MustCompile(`(?m)^kind:[\s]+([^\s]+)[\s]*$`)
	nameRegex = regexp.MustCompile(`(?m)^  name:[\s]+([^\s]+)[\s]*$`)
)

const generatedKind = "CustomResourceDefinition"

func extractKind(object string) (string, error) {
	matches := kindRegex.FindStringSubmatch(object)
	if len(matches) != 2 {
		return "", fmt.Errorf("object does not have exactly one kind")
	}
	return strings.Trim(matches[1], `"'`), nil
}

func extractName(object string) (string, error) {
	matches := nameRegex.FindStringSubmatch(object)
	if len(matches) != 2 {
		return "", fmt.Errorf("object does not have a top-level metadata.name")
	}
	return strings.Trim(matches[1], `"'`), nil
}

func extractCRDKind(object string) (string, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal([]byte(object), crd); err != nil {
		return "", fmt.Errorf("decode CRD: %w", err)
	}
	if crd.Spec.Names.Kind == "" {
		return "", fmt.Errorf("CRD does not define spec.names.kind")
	}
	return crd.Spec.Names.Kind, nil
}

type objectSet struct {
	byKind map[string][]string
}

func (set *objectSet) add(object string) error {
	kind, err := extractKind(object)
	if err != nil {
		return err
	}
	set.byKind[kind] = append(set.byKind[kind], object)
	return nil
}

func (set *objectSet) write() error {
	objects := append([]string(nil), set.byKind[generatedKind]...)
	sort.Slice(objects, func(i, j int) bool {
		left, leftErr := extractName(objects[i])
		right, rightErr := extractName(objects[j])
		if leftErr != nil || rightErr != nil {
			return objects[i] < objects[j]
		}
		return left < right
	})

	crdDir := filepath.Join(*outputDir, "crds")
	if err := os.MkdirAll(crdDir, 0o750); err != nil {
		return fmt.Errorf("create CRD output directory: %w", err)
	}

	seen := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		name, err := extractCRDKind(object)
		if err != nil {
			return err
		}
		filename := fmt.Sprintf("%s-customresourcedefinition.yaml", strings.ToLower(name))
		if _, exists := seen[filename]; exists {
			return fmt.Errorf("duplicate generated output filename: %s", filename)
		}
		seen[filename] = struct{}{}

		object = doReplacements(object)
		if strings.Contains(object, "HELMSUBST_") {
			return fmt.Errorf("unresolved Helm substitution in %s", filename)
		}
		destination := filepath.Join(crdDir, filename)
		if err := os.WriteFile(destination, []byte("---\n"+object), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", destination, err)
		}
	}
	return nil
}

func doReplacements(object string) string {
	keys := make([]string, 0, len(replacements))
	for old := range replacements {
		keys = append(keys, old)
	}
	sort.Strings(keys)
	for _, old := range keys {
		object = strings.ReplaceAll(object, old, replacements[old])
	}
	return object
}

func copyStaticFiles(root string, subdirs ...string) error {
	source := filepath.Join(append([]string{root}, subdirs...)...)
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read static chart directory %s: %w", source, err)
	}
	for _, entry := range entries {
		nextSubdirs := append(append([]string(nil), subdirs...), entry.Name())
		destination := filepath.Join(append([]string{*outputDir}, nextSubdirs...)...)
		if entry.IsDir() {
			if err := os.Mkdir(destination, 0o750); err != nil {
				return fmt.Errorf("create static output directory %s: %w", destination, err)
			}
			if err := copyStaticFiles(root, nextSubdirs...); err != nil {
				return err
			}
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported static chart entry: %s", filepath.Join(source, entry.Name()))
		}
		contents, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			return fmt.Errorf("read static chart file %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(destination, contents, 0o600); err != nil {
			return fmt.Errorf("write static chart file %s: %w", destination, err)
		}
	}
	return nil
}

func main() {
	flag.Parse()
	if err := os.MkdirAll(*outputDir, 0o750); err != nil {
		log.Fatalf("create chart output directory: %v", err)
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	objects := objectSet{byKind: make(map[string][]string)}
	var document strings.Builder
	flush := func() {
		if strings.TrimSpace(document.String()) == "" {
			document.Reset()
			return
		}
		object := document.String()
		document.Reset()
		if err := objects.add(object); err != nil {
			log.Fatalf("add generated object: %v", err)
		}
	}

	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "---" {
			flush()
			continue
		}
		document.WriteString(scanner.Text())
		document.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("read Kustomize output: %v", err)
	}
	flush()

	if err := copyStaticFiles(*staticDir); err != nil {
		log.Fatal(err)
	}
	if err := objects.write(); err != nil {
		log.Fatal(err)
	}
}
