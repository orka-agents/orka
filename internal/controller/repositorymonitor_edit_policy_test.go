package controller

import (
	"slices"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestRepositoryMonitorIssueNamedPaths(t *testing.T) {
	body := "The install steps in README.md are stale; see `docs/install.md:12` and " +
		"internal/controller/task.go#L40. Node.js 22 is fine, e.g. v1.2.3 too. " +
		"Not https://example.com/a/b.md, not `../etc/passwd`, not /abs/path.go, and `README.md` again."
	got := repositoryMonitorIssueNamedPaths("Fix README.md", body)
	want := []string{"docs/install.md", "README.md", "internal/controller/task.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("named paths = %q, want %q", got, want)
	}
	if got := repositoryMonitorIssueNamedPaths("Upgrade Node.js", "Bump to 1.2.3 and update the docs"); len(got) != 0 {
		t.Fatalf("named paths = %q, want none for prose", got)
	}
	if got := repositoryMonitorIssueNamedPaths("Contributing guide and Security policy", "See the License, the Makefile and `LICENSE`."); !slices.Equal(got, []string{"LICENSE", "Makefile"}) {
		t.Fatalf("named paths = %q, want only the distinctive and code-formatted names", got)
	}
	// Code names, bare file names whose directory is unknown, and owner/repo
	// names are not files; a directory in inline code needs its slash.
	code := "Use `http.Client` and `ctx.Err()` in `reconcile.go`, see `orka-agents/orka` and `v1.2.3`, then update `docs/guides/` and `internal/controller`. " +
		"Bump gopkg.in/yaml.v3 and `github.com/a/b.go`; Node.js/Deno and v1.2/v1.3 are prose, `.github/ISSUE_TEMPLATE/bug.yml` is a file."
	if got := repositoryMonitorIssueNamedPaths("Retry on timeout", code); !slices.Equal(got, []string{"docs/guides", ".github/ISSUE_TEMPLATE/bug.yml"}) {
		t.Fatalf("named paths = %q, want only the explicit directory and the hidden-directory file", got)
	}
	monitor := &corev1alpha1.RepositoryMonitor{}
	monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"internal/**"}
	item := &store.MonitorItem{Title: "Retry on timeout", Body: "Use `http.Client` and `ctx.Err()` in `reconcile.go`."}
	if policy := repositoryMonitorIssueImplementationEditPolicy(monitor, item, nil); policy.noEditableTarget() || len(policy.ReadOnlyFiles) != 0 {
		t.Fatalf("policy = %+v, want an issue naming only code to start", policy)
	}
}

func TestRepositoryMonitorEditTargetsAreBoundedRelativePaths(t *testing.T) {
	many := make([]string, 0, 30)
	for i := range 30 {
		many = append(many, "docs/page"+string(rune('a'+i%26))+string(rune('a'+i/26))+".md")
	}
	if got := repositoryMonitorEditTargets(many); len(got) != repositoryMonitorEditTargetLimit {
		t.Fatalf("targets = %d, want the limit %d", len(got), repositoryMonitorEditTargetLimit)
	}
	for _, rejected := range []string{"", "/etc/passwd", "../x.go", "a/../b.go", "-rf", "a//b.go", "https://x/y.go", "123", "a b.go"} {
		if target, ok := repositoryMonitorNormalizeEditTarget(rejected); ok {
			t.Errorf("normalize(%q) = %q, want rejected", rejected, target)
		}
	}
	if target, ok := repositoryMonitorNormalizeEditTarget("./docs/guide/"); !ok || target != "docs/guide" {
		t.Errorf("normalize(./docs/guide/) = %q, %v", target, ok)
	}
}

// A monitor that may edit only README.md gets an issue naming README.md and a
// plan naming two other files: README.md is editable and the plan's files are
// read-only context.
func TestRepositoryMonitorEditPolicySplitsEditableAndReadOnlyTargets(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{}
	monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"README.md", "docs/**"}
	records := []store.ActionRecord{
		{ActionKind: repositoryMonitorIssueActionPlan, PayloadJSON: `{"allowedFiles":[".github/ISSUE_TEMPLATE/bug_report.yml","website/docs/contributing.md"]}`},
		{ActionKind: repositoryMonitorIssueActionPlan, PayloadJSON: `{"allowedFiles":["stale-plan.md"]}`},
		{ActionKind: repositoryMonitorIssueActionResearch, PayloadJSON: `{"affectedFiles":["docs/a/b/c.md",".github/workflows/ci.yml"]}`},
	}
	targets := append(repositoryMonitorIssueNamedPaths("Update README.md", ""), repositoryMonitorIssuePlannedPaths(records)...)
	policy := newRepositoryMonitorEditPolicy(monitor, targets)
	if want := []string{"README.md", "docs/a/b/c.md"}; !slices.Equal(policy.EditableFiles, want) {
		t.Fatalf("editable = %q, want %q", policy.EditableFiles, want)
	}
	if want := []string{".github/ISSUE_TEMPLATE/bug_report.yml", "website/docs/contributing.md", ".github/workflows/ci.yml"}; !slices.Equal(policy.ReadOnlyFiles, want) {
		t.Fatalf("read-only = %q, want %q", policy.ReadOnlyFiles, want)
	}
	if policy.noEditableTarget() {
		t.Fatal("a policy with an editable target reported none")
	}

	none := newRepositoryMonitorEditPolicy(monitor, []string{"website/docs/contributing.md"})
	if !none.noEditableTarget() {
		t.Fatalf("policy %+v, want no editable target", none)
	}
	if empty := newRepositoryMonitorEditPolicy(monitor, nil); empty.noEditableTarget() {
		t.Fatal("an input naming no files reported no editable target")
	}
	unrestricted := newRepositoryMonitorEditPolicy(&corev1alpha1.RepositoryMonitor{}, []string{"website/docs/contributing.md", ".github/workflows/ci.yml"})
	if unrestricted.noEditableTarget() || !slices.Equal(unrestricted.ReadOnlyFiles, []string{".github/workflows/ci.yml"}) {
		t.Fatalf("unrestricted policy = %+v, want only the control path read-only", unrestricted)
	}
}

// The controller's patch check and the runtime share one matcher, so a
// pattern with a "**" between segments allows the same paths in both.
func TestRepositoryMonitorImplementationPathAllowedMatchesRuntimeGlobs(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{}
	monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"pkg/**/testdata/*.json"}
	if !repositoryMonitorImplementationPathAllowed(monitor, "pkg/a/b/testdata/x.json") {
		t.Fatal("mid-pattern ** did not match a nested path")
	}
	if repositoryMonitorImplementationPathAllowed(monitor, "pkg/a/b/x.json") {
		t.Fatal("pattern matched a path outside testdata")
	}
}
