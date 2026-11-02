package protectedset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/orka-agents/orka/internal/workspacedelta"
)

const (
	repositoryRoot     = "../.."
	sampleConfigMap    = "config/samples/orka_protected_set.yaml"
	orkaRepository     = "github.com/orka-agents/orka"
	validMinimalSetDoc = `schemaVersion: orka.protectedSet.v1
paths: [".github/**"]
gate: {maxInfraErrorPercent: 10, maxCandidatesPerBaseline: 3}
`
)

func loadSample(t *testing.T) (*corev1.ConfigMap, *Set) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot, sampleConfigMap))
	if err != nil {
		t.Fatalf("read sample protected set: %v", err)
	}
	var configMap corev1.ConfigMap
	if err := sigsyaml.UnmarshalStrict(data, &configMap); err != nil {
		t.Fatalf("decode sample ConfigMap: %v", err)
	}
	set, err := Parse([]byte(configMap.Data[ConfigMapKey]))
	if err != nil {
		t.Fatalf("Parse(sample) error = %v", err)
	}
	return &configMap, set
}

func TestSampleProtectedSetIsValid(t *testing.T) {
	_, set := loadSample(t)
	if set.SchemaVersion != SchemaVersion || len(set.CaseSets) != 2 || !set.HoldsCaseSets("orka-evals") || set.HoldsCaseSets("orka-proposers") {
		t.Fatalf("sample protected set = %#v", set)
	}
	_, again := loadSample(t)
	if !strings.HasPrefix(set.Digest(), "sha256:") || again.Digest() != set.Digest() {
		t.Fatalf("Digest() = %q, want a stable sha256 digest", set.Digest())
	}
}

// TestSampleProtectsEveryCategory covers each protected category with one
// representative path, for the repository and for any other repository.
func TestSampleProtectsEveryCategory(t *testing.T) {
	_, set := loadSample(t)
	denied := set.DeniedPathsFor("https://GitHub.com/Orka-Agents/Orka.git")
	for category, path := range map[string]string{
		"forge workflows":           ".github/workflows/ci.yml",
		"RBAC":                      "config/rbac/role.yaml",
		"charts":                    "charts/orka/values.yaml",
		"graders and gate":          "internal/evals/gate.go",
		"evaluation cases":          "test/evals/cases/pr_review.yaml",
		"API authorization":         "internal/api/external_authorization.go",
		"admission":                 "internal/admission/task_provenance.go",
		"outbound access":           "internal/outboundaccess/policy.go",
		"memory tools":              "internal/tools/memory_tools.go",
		"proposer tool allowlist":   "internal/controller/acp_mcp_broker_proposer.go",
		"dispatch-time checks":      "internal/controller/acp_dispatcher_proposer.go",
		"dispatch call site":        "internal/controller/acp_task_queue.go",
		"non-ACP execution gate":    "internal/controller/task_controller.go",
		"canary routing":            "internal/controller/agent_execution_binding.go",
		"evaluator":                 "internal/controller/evalrun_controller.go",
		"token and network":         "internal/controller/runtime_pool_controller.go",
		"implementation limits":     "internal/controller/repositorymonitor_issue_actions.go",
		"repair limits":             "internal/controller/repositorymonitor_repair.go",
		"publication boundary":      "internal/publisher/prepare.go",
		"records":                   "internal/store/sqlite/store.go",
		"supervisor path checks":    "workers/acp/supervisor/prompt_handlers.go",
		"API fields":                "api/v1alpha1/task_types.go",
		"protected-set declaration": "internal/protectedset/protectedset.go",
		"agent instructions":        "AGENTS.md",
	} {
		if !workspacedelta.PathDenied(denied, path) {
			t.Errorf("%s: %q is not protected", category, path)
		}
	}
	if workspacedelta.PathDenied(denied, "website/docs/intro.md") {
		t.Error("an unprotected documentation path is denied")
	}
	other := set.DeniedPathsFor("github.com/example/service")
	if !workspacedelta.PathDenied(other, ".github/workflows/ci.yml") || workspacedelta.PathDenied(other, "internal/admission/x.go") {
		t.Fatalf("protected set for another repository = %v, want only the global paths", other)
	}
}

func TestParseRejectsInvalidDeclarations(t *testing.T) {
	for name, document := range map[string]string{
		"empty":               "",
		"wrong schema":        strings.Replace(validMinimalSetDoc, "orka.protectedSet.v1", "orka.protectedSet.v2", 1),
		"unknown field":       validMinimalSetDoc + "extra: true\n",
		"misspelled paths":    "schemaVersion: orka.protectedSet.v1\npath: [\".github/**\"]\ngate: {maxInfraErrorPercent: 1, maxCandidatesPerBaseline: 1}\n",
		"no global paths":     "schemaVersion: orka.protectedSet.v1\npaths: []\ngate: {maxInfraErrorPercent: 1, maxCandidatesPerBaseline: 1}\n",
		"invalid pattern":     "schemaVersion: orka.protectedSet.v1\npaths: [\"../x\"]\ngate: {maxInfraErrorPercent: 1, maxCandidatesPerBaseline: 1}\n",
		"missing gate":        "schemaVersion: orka.protectedSet.v1\npaths: [\"a/**\"]\n",
		"gate out of range":   strings.Replace(validMinimalSetDoc, "maxInfraErrorPercent: 10", "maxInfraErrorPercent: 101", 1),
		"no candidates":       strings.Replace(validMinimalSetDoc, "maxCandidatesPerBaseline: 3", "maxCandidatesPerBaseline: 0", 1),
		"bad split":           validMinimalSetDoc + "caseSets: [{namespace: evals, name: cases, split: test}]\n",
		"bad namespace":       validMinimalSetDoc + "caseSets: [{namespace: Evals, name: cases, split: train}]\n",
		"duplicate case set":  validMinimalSetDoc + "caseSets: [{namespace: evals, name: cases, split: train}, {namespace: evals, name: cases, split: heldOut}]\n",
		"bad repository":      validMinimalSetDoc + "repositories: [{repository: orka, paths: [\"a/**\"]}]\n",
		"repository no paths": validMinimalSetDoc + "repositories: [{repository: github.com/a/b, paths: []}]\n",
		"duplicate repo":      validMinimalSetDoc + "repositories: [{repository: github.com/a/b, paths: [\"a\"]}, {repository: https://GitHub.com/A/B.git, paths: [\"b\"]}]\n",
	} {
		if _, err := Parse([]byte(document)); err == nil {
			t.Errorf("%s: Parse() accepted an invalid declaration", name)
		}
	}
}

func TestMissingDeniedPathsRequiresEveryPatternExactly(t *testing.T) {
	_, set := loadSample(t)
	required := set.DeniedPathsFor(orkaRepository)
	if missing := set.MissingDeniedPaths(required, orkaRepository); len(missing) != 0 {
		t.Fatalf("MissingDeniedPaths(full set) = %v, want none", missing)
	}
	withPrefix := make([]string, len(required))
	for index, pattern := range required {
		withPrefix[index] = "./" + pattern
	}
	if missing := set.MissingDeniedPaths(withPrefix, orkaRepository); len(missing) != 0 {
		t.Fatalf("MissingDeniedPaths(./-prefixed set) = %v, want none", missing)
	}
	// A broader glob does not stand in for a listed pattern.
	partial := append([]string{"**"}, required[1:]...)
	if missing := set.MissingDeniedPaths(partial, orkaRepository); !slices.Equal(missing, required[:1]) {
		t.Fatalf("MissingDeniedPaths(partial) = %v, want %v", missing, required[:1])
	}
	if missing := set.MissingDeniedPaths(set.Paths, orkaRepository); len(missing) == 0 {
		t.Fatal("global paths alone covered the repository's protected set")
	}
}

func TestSourceLoadFailsClosed(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sample, set := loadSample(t)
	configMap := func(name string, data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-system", Name: name}, Data: data}
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		configMap("valid", sample.Data),
		configMap("no-key", map[string]string{"other.yaml": validMinimalSetDoc}),
		configMap("invalid", map[string]string{ConfigMapKey: "schemaVersion: nope\n"}),
	).Build()

	for name, source := range map[string]Source{
		"unconfigured": {Reader: reader, Namespace: "orka-system"},
		"missing":      {Reader: reader, Namespace: "orka-system", Name: "absent"},
		"no key":       {Reader: reader, Namespace: "orka-system", Name: "no-key"},
		"invalid":      {Reader: reader, Namespace: "orka-system", Name: "invalid"},
	} {
		if _, err := source.Load(ctx); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: Load() error = %v, want ErrUnavailable", name, err)
		}
	}
	loaded, err := Source{Reader: reader, Namespace: "orka-system", Name: "valid"}.Load(ctx)
	if err != nil {
		t.Fatalf("Load(valid) error = %v", err)
	}
	if loaded.Digest() != set.Digest() {
		t.Fatalf("loaded digest = %s, want %s", loaded.Digest(), set.Digest())
	}
}
