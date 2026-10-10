package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type developmentFixture struct {
	*releaseFixture
	registry  map[string]string
	mutations []string
	mainSHA   string
}

func newDevelopmentFixture(t *testing.T) *developmentFixture {
	t.Helper()
	f := &developmentFixture{releaseFixture: newReleaseFixture(t), registry: map[string]string{}, mainSHA: testSHA}
	f.env["GITHUB_EVENT_NAME"] = "push"
	f.env["GITHUB_REF"] = "refs/heads/main"
	f.env["GITHUB_WORKFLOW_REF"] = repository + "/.github/workflows/development-images.yml@refs/heads/main"
	for i, name := range imageNames {
		digest := fmt.Sprintf("sha256:%064x", i+1)
		writeTestFile(t, filepath.Join(f.directory, "digests", "digest-"+name+".txt"), digest)
		f.registry[imageRepository(name)+"@"+digest] = digest
	}
	f.command = func(spec commandSpec) (commandResult, bool) {
		args := spec.args
		if len(args) < 5 || !slices.Equal(args[:3], []string{"docker", "buildx", "imagetools"}) {
			return commandResult{}, false
		}
		switch args[3] {
		case "inspect":
			digest, found := f.registry[args[4]]
			if !found {
				return commandResult{err: errors.New("missing"), stderr: "manifest unknown: not found"}, true
			}
			return commandResult{stdout: `{"digest":"` + digest + `"}`}, true
		case "create":
			if len(args) != 7 || args[4] != "--tag" {
				t.Fatalf("unexpected image promotion: %v", args)
			}
			f.registry[args[5]] = f.registry[args[6]]
			f.mutations = append(f.mutations, args[5])
			return commandResult{}, true
		default:
			t.Fatalf("unexpected registry operation: %v", args)
			return commandResult{}, true
		}
	}
	f.api = func(request apiRequest) (any, error, bool) {
		if request.path == repoAPI+"/git/ref/heads/main" {
			return gitRef{Object: gitObject{SHA: f.mainSHA}}, nil, true
		}
		return nil, nil, false
	}
	return f
}

func TestDevelopmentMatrixReusesCompleteReleaseImageInventory(t *testing.T) {
	var output bytes.Buffer
	w := newWorkflow(t.TempDir())
	w.stdout = &output
	must(t, w.execute([]string{"development-matrix"}))
	var matrix struct {
		Include []developmentImage `json:"include"`
	}
	must(t, json.Unmarshal(output.Bytes(), &matrix))
	if len(matrix.Include) != len(imageNames) {
		t.Fatalf("matrix contains %d images, want %d", len(matrix.Include), len(imageNames))
	}
	for i, image := range matrix.Include {
		if image.Name != imageNames[i] || image.Repository != imageRepository(image.Name) {
			t.Fatalf("unexpected matrix entry: %#v", image)
		}
		_, err := os.Stat(filepath.Join("..", "..", "..", image.Dockerfile))
		must(t, err)
	}
}

func TestDevelopmentPublicationPinsWholeSetBeforeMovingAliases(t *testing.T) {
	f := newDevelopmentFixture(t)
	must(t, f.w.execute([]string{"development-publish", filepath.Join(f.directory, "digests"), testSHA}))
	if len(f.mutations) != 2*len(imageNames) {
		t.Fatalf("unexpected promotions: %v", f.mutations)
	}
	for i, name := range imageNames {
		if f.mutations[i] != imageRepository(name)+":sha-"+testSHA ||
			f.mutations[len(imageNames)+i] != imageRepository(name)+":"+developmentVersion {
			t.Fatalf("image set was not promoted in phases: %v", f.mutations)
		}
	}
	var manifest struct {
		SourceSHA string            `json:"sourceSHA"`
		Images    map[string]string `json:"images"`
	}
	must(t, readJSON(filepath.Join(f.directory, "digests", "images.json"), &manifest))
	if manifest.SourceSHA != testSHA || len(manifest.Images) != len(imageNames) {
		t.Fatalf("invalid manifest: %#v", manifest)
	}
	for name, ref := range manifest.Images {
		if f.registry[imageRepository(name)+":sha-"+testSHA] != f.registry[ref] {
			t.Fatalf("pinned manifest differs from commit tag: %s", ref)
		}
	}
	var values map[string]any
	must(t, readJSON(filepath.Join(f.directory, "digests", "values.json"), &values))
	controller := values["controller"].(map[string]any)
	image := controller["image"].(map[string]any)
	if image["digest"] == "" || image["pullPolicy"] != "IfNotPresent" {
		t.Fatalf("controller is not pinned: %#v", image)
	}
	runtimes := controller["acpRuntime"].(map[string]any)
	for _, provider := range versionedRuntimeProviders {
		if runtimes[provider+"Image"] != manifest.Images["acp-"+provider+"-runtime"] {
			t.Fatalf("runtime not pinned: %s", provider)
		}
	}
	before := len(f.mutations)
	must(t, f.w.publishDevelopment(filepath.Join(f.directory, "digests"), testSHA))
	if len(f.mutations) != before {
		t.Fatal("retry rewrote an existing image tag")
	}
}

func TestDevelopmentPublicationRejectsIncompleteSetBeforeMutation(t *testing.T) {
	for _, invalid := range []string{"missing", "bad digest", "wrong image bytes", "conflicting commit tag"} {
		t.Run(invalid, func(t *testing.T) {
			f := newDevelopmentFixture(t)
			name := imageNames[len(imageNames)-1]
			path := filepath.Join(f.directory, "digests", "digest-"+name+".txt")
			switch invalid {
			case "missing":
				must(t, os.Remove(path))
			case "bad digest":
				writeTestFile(t, path, "sha256:bad")
			case "wrong image bytes":
				digest := strings.TrimSpace(readTestFile(t, path))
				f.registry[imageRepository(name)+"@"+digest] = testDigest
			case "conflicting commit tag":
				f.registry[imageRepository(name)+":sha-"+testSHA] = testDigest
			}
			if err := f.w.publishDevelopment(filepath.Join(f.directory, "digests"), testSHA); err == nil {
				t.Fatal("invalid set accepted")
			}
			if len(f.mutations) != 0 {
				t.Fatalf("partially published invalid set: %v", f.mutations)
			}
		})
	}
}

func TestDevelopmentPublicationRequiresExactTrustedMainPush(t *testing.T) {
	for key, bad := range map[string]string{
		"GITHUB_REPOSITORY": "other/orka", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/heads/feature",
		"GITHUB_SHA": strings.Repeat("b", 40), "GITHUB_RUN_ID": "",
		"GITHUB_WORKFLOW_REF": repository + "/other.yml@refs/heads/main",
	} {
		t.Run(key, func(t *testing.T) {
			f := newDevelopmentFixture(t)
			f.env[key] = bad
			if err := f.w.publishDevelopment(filepath.Join(f.directory, "digests"), testSHA); err == nil {
				t.Fatal("untrusted context accepted")
			}
			if len(f.mutations) != 0 {
				t.Fatal("untrusted context mutated registry")
			}
		})
	}
}

func TestDevelopmentPublicationDoesNotLetOlderBuildMoveAliases(t *testing.T) {
	f := newDevelopmentFixture(t)
	f.mainSHA = strings.Repeat("b", 40)
	must(t, f.w.publishDevelopment(filepath.Join(f.directory, "digests"), testSHA))
	if len(f.mutations) != len(imageNames) {
		t.Fatalf("obsolete run moved rolling aliases: %v", f.mutations)
	}
	for _, target := range f.mutations {
		if strings.HasSuffix(target, ":"+developmentVersion) {
			t.Fatal("obsolete build overwrote dev tag")
		}
	}
}

func TestDevelopmentPublicationFailsClosedOnRegistryAuthFailure(t *testing.T) {
	f := newDevelopmentFixture(t)
	f.command = func(spec commandSpec) (commandResult, bool) {
		if len(spec.args) > 0 && spec.args[0] == "docker" {
			return commandResult{err: errors.New("auth"), stderr: "unauthorized: not found"}, true
		}
		return commandResult{}, false
	}
	wantError(t, f.w.publishDevelopment(filepath.Join(f.directory, "digests"), testSHA), "could not inspect")
	if len(f.mutations) != 0 {
		t.Fatal("registry auth error caused a mutation")
	}
}

func TestDevelopmentPublicationRejectsChangedCheckout(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprint(dirty), func(t *testing.T) {
			f := newDevelopmentFixture(t)
			previous := f.command
			f.command = func(spec commandSpec) (commandResult, bool) {
				if slices.Equal(spec.args, []string{"git", "rev-parse", "HEAD"}) && !dirty {
					return commandResult{stdout: strings.Repeat("b", 40)}, true
				}
				if slices.Equal(spec.args, []string{"git", "status", "--porcelain"}) && dirty {
					return commandResult{stdout: " M changed.go"}, true
				}
				return previous(spec)
			}
			if err := f.w.publishDevelopment(filepath.Join(f.directory, "digests"), testSHA); err == nil {
				t.Fatal("changed checkout accepted")
			}
			if len(f.mutations) != 0 {
				t.Fatal("changed checkout mutated registry")
			}
		})
	}
}

func TestDevelopmentPublicationVerifiesTagReadback(t *testing.T) {
	f := newDevelopmentFixture(t)
	previous := f.command
	f.command = func(spec commandSpec) (commandResult, bool) {
		result, handled := previous(spec)
		if len(spec.args) > 3 && spec.args[0] == "docker" && spec.args[3] == "create" {
			f.registry[spec.args[5]] = testDigest
		}
		return result, handled
	}
	wantError(t, f.w.publishDevelopment(filepath.Join(f.directory, "digests"), testSHA), "does not match published bytes")
	for _, target := range f.mutations {
		if strings.HasSuffix(target, ":"+developmentVersion) {
			t.Fatal("bad commit-tag readback allowed rolling publication")
		}
	}
}

func TestDevelopmentWorkflowKeepsPublicationOnTrustedMain(t *testing.T) {
	body := readTestFile(t, filepath.Join("..", "..", "..", ".github", "workflows", "development-images.yml"))
	for _, required := range []string{
		"branches: [main]", "if: github.repository == 'orka-agents/orka'", "cancel-in-progress: false",
		"go run ./cmd/build/release development-matrix", "fromJSON(needs.prepare.outputs.matrix)",
		"needs: [prepare, build]", "linux/amd64,linux/arm64", "push-by-digest=true,name-canonical=true,push=true",
		"development-publish", "development-digest-*-${{ github.sha }}", "values.json", "images.json",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("development workflow omitted %q", required)
		}
	}
	for _, forbidden := range []string{"pull_request:", "pull_request_target:", "workflow_dispatch:", "tags:"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("development workflow enables an unintended publication path: %q", forbidden)
		}
	}
}

func TestDevelopmentWorkflowUploadsAreRetrySafe(t *testing.T) {
	body := readTestFile(t, filepath.Join("..", "..", "..", ".github", "workflows", "development-images.yml"))
	var document struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string `yaml:"uses"`
				With struct {
					Name      string `yaml:"name"`
					Overwrite bool   `yaml:"overwrite"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	must(t, yaml.Unmarshal([]byte(body), &document))
	uploads := 0
	for job, configuration := range document.Jobs {
		for _, step := range configuration.Steps {
			if !strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
				continue
			}
			uploads++
			if !step.With.Overwrite {
				t.Errorf("%s artifact %q cannot be replaced on rerun", job, step.With.Name)
			}
		}
	}
	if uploads != 2 {
		t.Fatalf("checked %d artifact uploads, want 2", uploads)
	}
}

func TestDevelopmentPinnedValuesRenderEveryBuiltImage(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for generated development values tests")
	}
	f := newDevelopmentFixture(t)
	directory := filepath.Join(f.directory, "digests")
	must(t, f.w.publishDevelopment(directory, testSHA))
	command := exec.Command(helm, "template", "development", "../helmify/static",
		"--values", filepath.Join(directory, "values.json"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("generated pinned values do not render: %v\n%s", err, output)
	}
	var manifest struct {
		Images map[string]string `json:"images"`
	}
	must(t, readJSON(filepath.Join(directory, "images.json"), &manifest))
	for name, ref := range manifest.Images {
		if name != harnessWrapperImage && !strings.Contains(string(output), ref) {
			t.Errorf("pinned chart does not select %s", ref)
		}
	}
	if strings.Contains(string(output), ":"+developmentVersion) {
		t.Error("pinned chart retains rolling image references")
	}
}
