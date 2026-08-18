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
		"cmd/build/helmify/static",
		"directory containing static Helm chart inputs",
	)
)

var kindRegex = regexp.MustCompile(`(?m)^kind:[\s]+([^\s]+)[\s]*$`)

type objectSet struct {
	crds []string
}

func (set *objectSet) add(object string) error {
	matches := kindRegex.FindStringSubmatch(object)
	if len(matches) != 2 {
		return fmt.Errorf("object does not have exactly one kind")
	}
	if strings.Trim(matches[1], `"'`) == "CustomResourceDefinition" {
		set.crds = append(set.crds, object)
	}
	return nil
}

func decodeCRD(object string) (*apiextensionsv1.CustomResourceDefinition, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal([]byte(object), crd); err != nil {
		return nil, fmt.Errorf("decode CRD: %w", err)
	}
	if crd.Spec.Group == "" || crd.Spec.Names.Kind == "" || crd.Spec.Names.Plural == "" {
		return nil, fmt.Errorf("CRD does not define group, kind, and plural names")
	}
	return crd, nil
}

func (set *objectSet) write() error {
	type generatedCRD struct {
		name     string
		filename string
		object   string
	}
	generated := make([]generatedCRD, 0, len(set.crds))
	seen := make(map[string]struct{}, len(set.crds))

	for _, object := range set.crds {
		crd, err := decodeCRD(object)
		if err != nil {
			return err
		}
		filename := fmt.Sprintf("%s-customresourcedefinition.yaml", strings.ToLower(crd.Spec.Names.Kind))
		if _, exists := seen[filename]; exists {
			return fmt.Errorf("duplicate generated output filename: %s", filename)
		}
		seen[filename] = struct{}{}
		generated = append(generated, generatedCRD{
			name:     crd.Spec.Group + "/" + crd.Spec.Names.Plural,
			filename: filename,
			object:   object,
		})
	}
	sort.Slice(generated, func(i, j int) bool { return generated[i].name < generated[j].name })

	crdDir := filepath.Join(*outputDir, "crds")
	if err := os.MkdirAll(crdDir, 0o750); err != nil {
		return fmt.Errorf("create CRD output directory: %w", err)
	}
	for _, crd := range generated {
		destination := filepath.Join(crdDir, crd.filename)
		if err := os.WriteFile(destination, []byte("---\n"+crd.object), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", destination, err)
		}
	}
	return nil
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
	objects := objectSet{}
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
