package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

const (
	// repositoryMonitorEditTargetLimit bounds the files an edit policy lists.
	repositoryMonitorEditTargetLimit = 20
	// repositoryMonitorEditTargetScanLimit bounds the issue text scanned for
	// file names.
	repositoryMonitorEditTargetScanLimit = 64 * 1024
	repositoryMonitorEditTargetMaxLength = 256
)

// repositoryMonitorEditPolicy tells an implement or repair Task which of the
// files its input names it may change. EditableFiles pass the monitor's
// allowed paths and are not repository-control paths; ReadOnlyFiles are
// context the Task may read but not change.
type repositoryMonitorEditPolicy struct {
	AllowedPaths                 []string `json:"allowedPaths,omitempty"`
	DeniesRepositoryControlPaths bool     `json:"deniesRepositoryControlPaths"`
	EditableFiles                []string `json:"editableFiles,omitempty"`
	ReadOnlyFiles                []string `json:"readOnlyFiles,omitempty"`
}

// restricted reports whether the policy limits edits to allowed paths.
func (p repositoryMonitorEditPolicy) restricted() bool {
	return len(p.AllowedPaths) > 0
}

// noEditableTarget reports whether the input names files but the policy lets
// the Task change none of them.
func (p repositoryMonitorEditPolicy) noEditableTarget() bool {
	return p.restricted() && len(p.EditableFiles) == 0 && len(p.ReadOnlyFiles) > 0
}

// repositoryMonitorPathEditable reports whether a write Task of monitor may
// change path. It applies the rules the runtime enforces on the workspace
// delta, so a file listed as editable is never rejected after the run.
func repositoryMonitorPathEditable(monitor *corev1alpha1.RepositoryMonitor, path string) bool {
	if harnessv2.WorkspaceDeltaRepositoryControlPath(path) {
		return false
	}
	return repositoryMonitorImplementationPathAllowed(monitor, path)
}

// newRepositoryMonitorEditPolicy splits targets into the files monitor lets a
// write Task change and the files it may only read.
func newRepositoryMonitorEditPolicy(monitor *corev1alpha1.RepositoryMonitor, targets []string) repositoryMonitorEditPolicy {
	policy := repositoryMonitorEditPolicy{DeniesRepositoryControlPaths: true}
	if monitor != nil {
		for _, pattern := range monitor.Spec.IssueWorkflow.Implementation.AllowedPaths {
			if pattern = strings.TrimSpace(pattern); pattern != "" {
				policy.AllowedPaths = append(policy.AllowedPaths, pattern)
			}
		}
	}
	for _, target := range repositoryMonitorEditTargets(targets) {
		if repositoryMonitorPathEditable(monitor, target) {
			policy.EditableFiles = append(policy.EditableFiles, target)
		} else {
			policy.ReadOnlyFiles = append(policy.ReadOnlyFiles, target)
		}
	}
	return policy
}

// repositoryMonitorEditTargets keeps the path-shaped entries of paths,
// normalized, deduplicated, in order, and bounded.
func repositoryMonitorEditTargets(paths []string) []string {
	out := make([]string, 0, min(len(paths), repositoryMonitorEditTargetLimit))
	seen := map[string]struct{}{}
	for _, raw := range paths {
		target, ok := repositoryMonitorNormalizeEditTarget(raw)
		if !ok {
			continue
		}
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
		if len(out) == repositoryMonitorEditTargetLimit {
			break
		}
	}
	return out
}

var (
	repositoryMonitorEditTargetPattern   = regexp.MustCompile(`^[A-Za-z0-9._+@-]+(/[A-Za-z0-9._+@-]+)*$`)
	repositoryMonitorEditTargetLocSuffix = regexp.MustCompile(`(:\d+(-\d+)?(:\d+)?|#L\d+(-L?\d+)?)$`)
	repositoryMonitorInlineCodePattern   = regexp.MustCompile("`([^`\n]{1,256})`")
)

// repositoryMonitorNormalizeEditTarget returns raw as a repository-relative
// path, without a leading "./", a trailing "/", or a line suffix such as
// ":12" or "#L12". It rejects anything that is not a plain relative path.
func repositoryMonitorNormalizeEditTarget(raw string) (string, bool) {
	target := strings.TrimSpace(raw)
	if target == "" || len(target) > repositoryMonitorEditTargetMaxLength || strings.Contains(target, "://") {
		return "", false
	}
	target = repositoryMonitorEditTargetLocSuffix.ReplaceAllString(target, "")
	target = strings.TrimSuffix(strings.TrimPrefix(target, "./"), "/")
	if !repositoryMonitorEditTargetPattern.MatchString(target) || !strings.ContainsAny(target, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		return "", false
	}
	for segment := range strings.SplitSeq(target, "/") {
		if segment == "." || segment == ".." || strings.HasPrefix(segment, "-") {
			return "", false
		}
	}
	return target, true
}

// repositoryMonitorRootFileNames are file names an issue may mention without a
// directory or code formatting and still mean the repository file.
var repositoryMonitorRootFileNames = map[string]struct{}{
	"readme": {}, "changelog": {}, "contributing": {}, "license": {}, "security": {}, "code_of_conduct": {},
	"codeowners": {}, "maintainers": {}, "governance": {}, "authors": {}, "notice": {}, "support": {},
	"makefile": {}, "dockerfile": {}, "go.mod": {}, "go.sum": {}, "package.json": {}, "pyproject.toml": {},
	"cargo.toml": {}, "requirements.txt": {},
}

// repositoryMonitorIssueNamedPaths returns the repository files that an issue's
// title or body names unambiguously: a path with a directory whose last
// segment has an extension, a directory written with a trailing slash in
// inline code, or a well-known root file name such as README.md. Code names
// such as `http.Client`, bare file names such as `reconcile.go` whose
// directory is unknown, and owner/repo names are not files. The issue text is
// untrusted; it only adds candidate targets, which the edit policy still
// classifies.
func repositoryMonitorIssueNamedPaths(title, body string) []string {
	text := title + "\n" + body
	if len(text) > repositoryMonitorEditTargetScanLimit {
		text = text[:repositoryMonitorEditTargetScanLimit]
	}
	var candidates []string
	for _, match := range repositoryMonitorInlineCodePattern.FindAllStringSubmatch(text, -1) {
		raw := strings.TrimSpace(match[1])
		target, ok := repositoryMonitorNormalizeEditTarget(raw)
		if !ok {
			continue
		}
		explicitDirectory := strings.HasSuffix(raw, "/") && !strings.Contains(raw, "://")
		if repositoryMonitorDirectoryQualifiedFile(target) || explicitDirectory || repositoryMonitorRootFileName(target, false) {
			candidates = append(candidates, target)
		}
	}
	for word := range strings.FieldsSeq(repositoryMonitorInlineCodePattern.ReplaceAllString(text, " ")) {
		word = strings.Trim(word, "()[]{}<>\"'`,;:!?*")
		word = strings.TrimSuffix(word, ".")
		target, ok := repositoryMonitorNormalizeEditTarget(word)
		if !ok {
			continue
		}
		if repositoryMonitorRootFileName(target, true) || repositoryMonitorDirectoryQualifiedFile(target) {
			candidates = append(candidates, target)
		}
	}
	return repositoryMonitorEditTargets(candidates)
}

// repositoryMonitorDirectoryQualifiedFile reports whether target names a file
// inside a directory: it has a "/" and its last segment has an extension. A
// first segment with a dot that is not a hidden directory, as in
// gopkg.in/yaml.v3, github.com/a/b.go, or Node.js/Deno, is a module path or
// prose rather than a repository directory.
func repositoryMonitorDirectoryQualifiedFile(target string) bool {
	slash := strings.LastIndex(target, "/")
	if slash < 0 || !strings.Contains(target[slash+1:], ".") {
		return false
	}
	first, _, _ := strings.Cut(target, "/")
	return strings.HasPrefix(first, ".") || !strings.Contains(first, ".")
}

func repositoryMonitorRootFileName(target string, plainWord bool) bool {
	if strings.Contains(target, "/") {
		return false
	}
	name := strings.ToLower(target)
	if _, ok := repositoryMonitorRootFileNames[name]; ok {
		// In prose, "License" or "Security" is a word, not a file; only
		// distinctive names without an extension count there.
		_, distinctive := repositoryMonitorDistinctiveRootFileNames[name]
		return !plainWord || strings.Contains(name, ".") || distinctive
	}
	stem, _, _ := strings.Cut(name, ".")
	_, ok := repositoryMonitorRootFileNames[stem]
	return ok && stem != name
}

// repositoryMonitorDistinctiveRootFileNames are root file names without an
// extension that prose rarely uses as plain words.
var repositoryMonitorDistinctiveRootFileNames = map[string]struct{}{"makefile": {}, "dockerfile": {}, "codeowners": {}}

// repositoryMonitorIssuePlannedPaths returns the files the latest plan's
// allowedFiles and the latest research's affectedFiles name. Both are model
// output; they only add candidate targets, which the edit policy classifies.
func repositoryMonitorIssuePlannedPaths(records []store.ActionRecord) []string {
	var planFiles, researchFiles []string
	planSeen, researchSeen := false, false
	for _, record := range records {
		switch record.ActionKind {
		case repositoryMonitorIssueActionPlan:
			if planSeen {
				continue
			}
			planSeen = true
			planFiles = repositoryMonitorPayloadStrings(record.PayloadJSON, "allowedFiles")
		case repositoryMonitorIssueActionResearch:
			if researchSeen {
				continue
			}
			researchSeen = true
			researchFiles = repositoryMonitorPayloadStrings(record.PayloadJSON, "affectedFiles")
		}
	}
	return append(planFiles, researchFiles...)
}

func repositoryMonitorPayloadStrings(payloadJSON, field string) []string {
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(payloadJSON), &payload) != nil {
		return nil
	}
	var values []string
	if json.Unmarshal(payload[field], &values) != nil {
		return nil
	}
	return values
}

const (
	// repositoryMonitorNoEditableTargetReason refuses a write Task whose input
	// names files that the edit policy lets it change none of.
	repositoryMonitorNoEditableTargetReason = "no_editable_target"
	repositoryMonitorIssueActionRefused     = "issue_action_refused"
	repositoryMonitorEditPolicyField        = "editPolicy"
	repositoryMonitorReadOnlyFilesField     = "readOnlyFiles"

	// repositoryMonitorEditPolicyInstruction tells implement and repair Tasks
	// how to use the editPolicy in their input.
	repositoryMonitorEditPolicyInstruction = " Change only files the input's editPolicy allows: editableFiles are named files you may change, and readOnlyFiles are context to read but not change. When editPolicy.allowedPaths is set, every file you change must match it, and repository-control paths (.github/workflows/, config/rbac/, and chart files about secrets) are never changeable; a change outside the policy is rejected after the run."
	// repositoryMonitorPlanEditPolicyInstruction keeps a plan's allowedFiles
	// inside the paths implementation may change.
	repositoryMonitorPlanEditPolicyInstruction = " Keep allowedFiles inside the input's editPolicy: when editPolicy.allowedPaths is set, list only files that match it, and never list repository-control paths (.github/workflows/, config/rbac/, or chart files about secrets). Mention other relevant files in steps as read-only context."
)

// repositoryMonitorIssueImplementationEditPolicy classifies the files an
// issue's text, latest plan, and latest research name for an implement Task.
func repositoryMonitorIssueImplementationEditPolicy(monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, priorActions []store.ActionRecord) repositoryMonitorEditPolicy {
	var targets []string
	if item != nil {
		targets = repositoryMonitorIssueNamedPaths(item.Title, item.Body)
	}
	return newRepositoryMonitorEditPolicy(monitor, append(targets, repositoryMonitorIssuePlannedPaths(priorActions)...))
}

// repositoryMonitorRepairEditTargets returns the files a repair's input
// names: open review findings, external review threads, and CI annotations.
func repositoryMonitorRepairEditTargets(findings []repositoryMonitorReviewFinding, threads []repositoryMonitorExternalReviewThread, ciInput *repositoryMonitorRepairCIInput) []string {
	var targets []string
	for _, finding := range findings {
		targets = append(targets, finding.File)
	}
	for _, thread := range threads {
		targets = append(targets, thread.Path)
	}
	if ciInput != nil {
		for _, check := range ciInput.Evidence.Checks {
			for _, annotation := range check.Annotations {
				targets = append(targets, annotation.Path)
			}
		}
	}
	return targets
}

// repositoryMonitorNoEditableTargetError stops a repair before its Task is
// created because its input names only files the Task may not change.
type repositoryMonitorNoEditableTargetError struct {
	policy repositoryMonitorEditPolicy
}

// repositoryMonitorRepairRefusesNoEditableTarget reports whether a repair is
// refused before its Task. A findings repair edits where its findings point,
// so it is refused when every file its input names is read-only. CI and
// branch-update repairs never are: CI annotations show where a failure
// surfaces, not where the fix goes.
func repositoryMonitorRepairRefusesNoEditableTarget(intent string, policy repositoryMonitorEditPolicy) bool {
	return intent == repositoryMonitorCommandIntentFix && policy.noEditableTarget()
}

func (e *repositoryMonitorNoEditableTargetError) Error() string {
	return "repair input names no file the monitor's allowed paths let it change"
}

func repositoryMonitorNoEditableTargetSummary(subject string, policy repositoryMonitorEditPolicy) string {
	files := make([]string, 0, len(policy.ReadOnlyFiles))
	for _, file := range policy.ReadOnlyFiles {
		files = append(files, "`"+file+"`")
	}
	return subject + " was not started: none of the files it names can be changed under this monitor's allowed paths. Read-only: " + strings.Join(files, ", ") + "."
}

// refuseRepositoryMonitorIssueImplementation records that an implementation
// was refused before its Task, because nothing the issue or its plan names is
// editable, and shows the refusal in the issue's status comment.
func (r *RepositoryMonitorReconciler) refuseRepositoryMonitorIssueImplementation(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, command *store.CommandEvent, item *store.MonitorItem, policy repositoryMonitorEditPolicy) error {
	item.WorkflowPhase = repositoryMonitorIssuePhaseBlocked
	item.SkipReason = repositoryMonitorNoEditableTargetReason
	if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorIssueKind, item.Number, "", item.SnapshotDigest, repositoryMonitorIssueActionImplementation, repositoryMonitorWorkActionStatusBlocked, item.WorkflowPhase, "", item.SkipReason); err != nil {
		return err
	}
	if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
		return err
	}
	metadata := map[string]any{
		repositoryMonitorEventActionKindKey: repositoryMonitorIssueActionImplementation,
		eventReasonField:                    repositoryMonitorNoEditableTargetReason,
		repositoryMonitorReadOnlyFilesField: policy.ReadOnlyFiles,
	}
	commandID := ""
	if command != nil {
		commandID = command.ID
		metadata[commandEventIDField] = command.ID
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	if err := r.createMonitorEvent(ctx, monitor, runID, repositoryMonitorIssueKind, item.Number, item.SnapshotDigest, repositoryMonitorIssueActionRefused, fmt.Sprintf("Issue #%d implementation refused: %s", item.Number, repositoryMonitorNoEditableTargetReason), metadata); err != nil {
		return err
	}
	// The record only renders the status comment; its ID keys the comment
	// mutation, so a replayed refusal updates the comment once.
	record := &store.ActionRecord{
		ID:             "refusal-" + repositoryMonitorShortHash(strings.Join([]string{monitor.Namespace, monitor.Name, strconv.FormatInt(item.Number, 10), item.SnapshotDigest, commandID, repositoryMonitorNoEditableTargetReason}, "|")),
		CommandEventID: commandID,
		ActionKind:     repositoryMonitorIssueActionImplementation,
		Summary:        repositoryMonitorNoEditableTargetSummary("Implementation", policy),
	}
	return r.upsertRepositoryMonitorIssueStatusComment(ctx, monitor, item, record)
}

// refuseRepositoryMonitorRepairNoEditableTarget records that a findings repair
// was refused before its Task because no finding names an editable file.
func (r *RepositoryMonitorReconciler) refuseRepositoryMonitorRepairNoEditableTarget(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, command *store.CommandEvent, pr repositoryMonitorPullRequest, item *store.MonitorItem, policy repositoryMonitorEditPolicy) error {
	item.RepairState = repositoryMonitorRepairPhaseFailed
	item.SkipReason = repositoryMonitorNoEditableTargetReason
	if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", repositoryMonitorRepairWorkflowActionKind(command.Intent), repositoryMonitorWorkActionStatusBlocked, "repair_refused", "", repositoryMonitorNoEditableTargetReason); err != nil {
		return err
	}
	if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
		return err
	}
	metadata := map[string]any{
		intentField:                         command.Intent,
		eventReasonField:                    repositoryMonitorNoEditableTargetReason,
		commandEventIDField:                 command.ID,
		repositoryMonitorReadOnlyFilesField: policy.ReadOnlyFiles,
	}
	return r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, repositoryMonitorEventRepairRefused, fmt.Sprintf("Pull request #%d repair refused: %s", pr.Number, repositoryMonitorNoEditableTargetReason), metadata)
}
