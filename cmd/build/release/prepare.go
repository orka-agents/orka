package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type environmentConfig struct {
	BranchPolicy *struct {
		Custom bool `json:"custom_branch_policies"`
	} `json:"deployment_branch_policy"`
	CanAdminsBypass *bool `json:"can_admins_bypass"`
	Rules           []struct {
		Type      string            `json:"type"`
		Reviewers []json.RawMessage `json:"reviewers"`
	} `json:"protection_rules"`
}

type branchPolicy struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func (w *workflow) checkEnvironment(name, branch string) error {
	endpoint := repoAPI + "/environments/" + name
	var config environmentConfig
	if err := w.api(endpoint, http.MethodGet, nil, &config); err != nil {
		return err
	}
	if config.BranchPolicy == nil || !config.BranchPolicy.Custom {
		return fmt.Errorf("%s must restrict deployments to explicitly selected branches", name)
	}
	policies, err := paginated[branchPolicy](w, endpoint+"/deployment-branch-policies?per_page=100", "branch_policies")
	if err != nil {
		return err
	}
	if !slices.Contains(policies, branchPolicy{Type: policyBranch, Name: branch}) {
		return fmt.Errorf("add the exact %s branch to the %s environment; wildcard rules are insufficient", branch, name)
	}
	var defaultBranch string
	if name == qualificationEnvironment {
		defaultBranch, err = w.defaultBranch()
		if err != nil {
			return err
		}
	}
	for _, policy := range policies {
		allowed := branchRE.MatchString(policy.Name) || (defaultBranch != "" && policy.Name == defaultBranch)
		if policy.Type != policyBranch || !allowed {
			return fmt.Errorf("remove wildcard, tag, and unrelated branch rules from the %s environment", name)
		}
	}
	if name == "release" || name == qualificationEnvironment {
		reviewer := slices.ContainsFunc(config.Rules, func(rule struct {
			Type      string            `json:"type"`
			Reviewers []json.RawMessage `json:"reviewers"`
		}) bool {
			return rule.Type == "required_reviewers" && len(rule.Reviewers) != 0
		})
		if !reviewer {
			return fmt.Errorf("the %s environment must have a required reviewer before starting a release", name)
		}
		if config.CanAdminsBypass == nil || *config.CanAdminsBypass {
			return fmt.Errorf("disable administrator bypass for the %s environment", name)
		}
	}
	return nil
}

type replacement struct {
	pattern string
	value   string
	count   int
}

func versionFields() map[string][]replacement {
	bare := strings.TrimPrefix(versionPattern, "v")
	fields := map[string][]replacement{
		makefilePath: {{pattern: `^VERSION := ` + versionPattern + `$`, value: "VERSION := RELEASE_VERSION"}},
		chartInputPath: {
			{pattern: `^version: ` + bare + `$`, value: "version: RELEASE_VERSION"},
			{pattern: `^appVersion: "` + versionPattern + `"$`, value: `appVersion: "RELEASE_VERSION"`},
		},
	}
	for _, name := range versionedImages {
		fields[valuesInputPath] = append(fields[valuesInputPath], replacement{
			pattern: `^([ \t]+repository:[ \t]*` + regexp.QuoteMeta(imageRepository(name)) + `[ \t]*\n` +
				`(?:[ \t]*(?:#.*)?\n)*[ \t]+tag: )"` + bare + `"$`,
			value: `${1}"RELEASE_VERSION"`,
		})
	}
	return fields
}

func (w *workflow) inputIdentity(commit, path string, replacements []replacement) (string, error) {
	entry, err := w.command(gitCommand, "ls-tree", commit, "--", path)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(entry)
	if len(fields) != 4 || fields[1] != "blob" {
		return "", errors.New("release input is missing")
	}
	content, err := w.commandRaw(w.root, "", gitCommand, "show", commit+":"+path)
	if err != nil {
		return "", err
	}
	for _, edit := range replacements {
		content = regexp.MustCompile("(?m)"+edit.pattern).ReplaceAllString(content, edit.value)
	}
	return fields[0] + "\x00" + content, nil
}

func (w *workflow) checkPreparationAutomation(trusted, candidate string) error {
	if !shaRE.MatchString(trusted) || !shaRE.MatchString(candidate) {
		return errors.New("invalid preparation source identity")
	}
	// The release command and all its Go sources stay within cmd/build. Any new
	// repository package it imports must be included in this trusted comparison.
	paths := []string{
		".github", "scripts", "cmd/build", ".agents/skills/kindctl", ".agents/skills/vekil-reverse-proxy-deploy",
		"bin", "vendor", "go.mod", "go.sum", "go.work", "go.work.sum", makefilePath, "GNUmakefile", "makefile",
	}
	args := append([]string{gitCommand, gitDiff, "--name-only", "-z", "--no-renames", trusted, candidate, "--"}, paths...)
	raw, err := w.commandRaw(w.root, "", args...)
	if err != nil {
		return err
	}
	fields := versionFields()
	for _, path := range nullPaths(raw) {
		if edits, ok := fields[path]; ok {
			before, err := w.inputIdentity(trusted, path, edits)
			if err != nil {
				return err
			}
			after, err := w.inputIdentity(candidate, path, edits)
			if err != nil {
				return err
			}
			if before == after {
				continue
			}
		}
		return errors.New("release automation differs from the dispatched default-branch commit; " +
			"backport the reviewed workflows, scripts, generator, chart inputs, and toolchain before preparation")
	}
	return nil
}

func nullPaths(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
}

func (w *workflow) preparationBase(branch, trusted string) (base, lease string, err error) {
	runs, err := paginated[workflowRun](w, repoAPI+"/actions/workflows/"+releaseWorkflow+
		"/runs?branch="+branch+"&per_page=100", "workflow_runs")
	if err != nil {
		return "", "", err
	}
	for _, run := range runs {
		if run.Status != runCompleted {
			return "", "", errors.New("another release is still running for this branch; finish or cancel it first")
		}
	}
	var refs []gitRef
	if err := w.api(repoAPI+"/git/matching-refs/heads/"+branch, http.MethodGet, nil, &refs); err != nil {
		return "", "", err
	}
	for _, ref := range refs {
		if ref.Ref != "refs/heads/"+branch {
			continue
		}
		if _, err := w.command(gitCommand, "fetch", "origin", "refs/heads/"+branch); err != nil {
			return "", "", err
		}
		if err := w.checkPreparationAutomation(trusted, ref.Object.SHA); err != nil {
			return "", "", err
		}
		if _, err := w.command(gitCommand, "checkout", "--detach", ref.Object.SHA); err != nil {
			return "", "", err
		}
		return ref.Object.SHA, ref.Object.SHA, nil
	}
	return trusted, "", nil
}

func (w *workflow) generateCandidate(version string) (string, error) {
	info, err := os.Stat(filepath.Join(w.root, "cmd/build/release/main.go"))
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("backport release automation to this branch first")
	}
	commands := [][]string{
		{makeCommand, "release-manifest", "NEWVERSION=" + version}, {makeCommand, "promote-staging-manifest"},
		{makeCommand, "verify-release-manifest", "NEWVERSION=" + version}, {gitCommand, gitDiff, "--check"},
	}
	for _, args := range commands {
		// These build commands carry no credentials; retain their useful diagnostics.
		if result := w.run(commandSpec{dir: w.root, args: args, stream: true}); result.err != nil {
			return "", fmt.Errorf("%s failed", args[0])
		}
	}
	allowed := []string{makefilePath, chartInputPath, valuesInputPath,
		"config/manager/manager.yaml", "config/manager/kustomization.yaml"}
	for _, args := range [][]string{
		{gitCommand, gitDiff, "--name-only", "-z"}, {gitCommand, "ls-files", "--others", "--exclude-standard", "-z"},
	} {
		raw, err := w.commandRaw(w.root, "", args...)
		if err != nil {
			return "", err
		}
		for _, path := range nullPaths(raw) {
			if !slices.Contains(allowed, path) && !strings.HasPrefix(path, "manifest_staging/") &&
				!strings.HasPrefix(path, "deploy/") && !strings.HasPrefix(path, "charts/orka/") {
				return "", errors.New("release generation changed an unexpected source file")
			}
		}
	}
	args := append([]string{gitCommand, "add", "--"}, allowed...)
	args = append(args, "manifest_staging", "deploy", "charts/orka")
	if _, err := w.command(args...); err != nil {
		return "", err
	}
	changed, err := w.command(gitCommand, gitDiff, "--cached", "--name-only")
	if err != nil {
		return "", err
	}
	if changed != "" {
		if _, err := w.command(botCommit("chore(release): prepare " + version)...); err != nil {
			return "", err
		}
	}
	return w.command(gitCommand, "rev-parse", "HEAD")
}

func botCommit(message string) []string {
	return []string{gitCommand, "-c", "user.name=github-actions[bot]", "-c",
		"user.email=41898282+github-actions[bot]@users.noreply.github.com", gitCommit, "-s", "-m", message}
}

func (w *workflow) prepare(version string) error {
	if err := w.checkRunIdentity(); err != nil {
		return err
	}
	branch, err := branchFor(version)
	if err != nil {
		return err
	}
	defaultBranch, err := w.defaultBranch()
	if err != nil {
		return err
	}
	trusted := w.env("GITHUB_SHA")
	if err := w.checkContext(defaultBranch, trusted); err != nil {
		return err
	}
	status, err := w.command(gitCommand, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("preparation requires a clean checkout")
	}
	if err := w.checkCandidateEnvironment(version, branch); err != nil {
		return err
	}
	base, lease, err := w.preparationBase(branch, trusted)
	if err != nil {
		return err
	}
	candidate, err := w.generateCandidate(version)
	if err != nil {
		return err
	}
	args := append(slices.Clone(gitAuth), "push", "--force-with-lease=refs/heads/"+branch+":"+lease,
		"origin", candidate+":refs/heads/"+branch)
	if _, err := w.command(args...); err != nil {
		return fmt.Errorf("push to %s failed; check branch rules and the native token's Contents write permission; "+
			"the expected branch head may also have changed", branch)
	}
	text := fmt.Sprintf("Prepared `%s` at `%s` on `%s`.\n\n"+
		"[Review generated changes](https://github.com/%s/compare/%s...%s).",
		version, candidate, branch, repository, base, candidate)
	if base != trusted {
		text += fmt.Sprintf("\n\n[Review release-line source changes](https://github.com/%s/compare/%s...%s) "+
			"before approving qualification. Release tooling matches the dispatched default-branch commit.",
			repository, trusted, base)
	}
	if err := w.summary(text); err != nil {
		return err
	}
	run, err := w.dispatch(releaseWorkflow, branch, candidate, map[string]string{
		"release_version": version, "candidate_sha": candidate,
		"dispatch_id": "prepare-" + w.env("GITHUB_RUN_ID") + "-" + w.env("GITHUB_RUN_ATTEMPT"),
	}, false)
	if err != nil {
		return err
	}
	return w.summary("[Follow validation and release approval](" + run.HTMLURL + ").")
}

func (w *workflow) checkCandidateEnvironment(version, branch string) error {
	for _, environment := range []string{"release", qualificationEnvironment} {
		if err := w.checkEnvironment(environment, branch); err != nil {
			return err
		}
	}
	tag, err := w.tagRef(version)
	if err != nil {
		return err
	}
	if tag != nil {
		return errors.New("version is already tagged; retry failed publication jobs in the original run")
	}
	return nil
}

func (w *workflow) validateCandidate(version, candidate string) error {
	if err := w.checkRunIdentity(); err != nil {
		return err
	}
	branch, err := branchFor(version)
	if err != nil {
		return err
	}
	if err := w.checkContext(branch, candidate); err != nil {
		return err
	}
	if err := w.checkCandidateEnvironment(version, branch); err != nil {
		return err
	}
	return w.output("artifact_attempt", w.env("GITHUB_RUN_ATTEMPT"))
}

func (w *workflow) checkRunIdentity() error {
	if !positiveNumber(w.env("GITHUB_RUN_ID")) || !positiveNumber(w.env("GITHUB_RUN_ATTEMPT")) {
		return errors.New("invalid workflow run identity")
	}
	return nil
}
