package main

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type releaseArtifact struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type candidateBundle struct {
	SchemaVersion   int               `json:"schemaVersion"`
	Repository      string            `json:"repository"`
	Version         string            `json:"version"`
	CandidateSHA    string            `json:"candidateSHA"`
	Branch          string            `json:"branch"`
	BuildRunID      string            `json:"buildRunID"`
	BuildRunAttempt string            `json:"buildRunAttempt"`
	Images          map[string]string `json:"images"`
	Chart           releaseArtifact   `json:"chart"`
	CLI             []releaseArtifact `json:"cli"`
}

type qualificationProof struct {
	RunID            string `json:"runID"`
	RunAttempt       string `json:"runAttempt"`
	AcceptanceSHA256 string `json:"acceptanceSHA256"`
	CandidateSHA256  string `json:"candidateSHA256"`
}

type observedImage struct {
	RequestedImage string `json:"requestedImage"`
}

type chartAcceptance struct {
	PackageSHA256        string `json:"packageSHA256"`
	Install              bool   `json:"install"`
	ContainerTask        bool   `json:"containerTask"`
	Recovery             bool   `json:"recovery"`
	NoReplay             bool   `json:"noReplay"`
	OppositeModeRejected bool   `json:"oppositeModeRejected"`
}

type acceptanceReport struct {
	Result       string `json:"result"`
	CandidateSHA string `json:"candidateSHA"`
	// Exact keys are part of the release binding; reject additional fields here.
	Release     map[string]string        `json:"release"`
	BuiltImages map[string]string        `json:"builtImages"`
	Images      map[string]observedImage `json:"images"`
	Chart       chartAcceptance          `json:"chart"`
}

type runArtifact struct {
	Name    string `json:"name"`
	Expired *bool  `json:"expired,omitempty"`
}

func (w *workflow) bundle(directory, version, candidate, attempt string) error {
	branch, err := branchFor(version)
	if err != nil {
		return err
	}
	if !shaRE.MatchString(candidate) {
		return errors.New("invalid candidate SHA")
	}
	if !positiveNumber(attempt) {
		return errors.New("invalid build attempt")
	}
	if !positiveNumber(w.env("GITHUB_RUN_ID")) {
		return errors.New("invalid build run identity")
	}
	images := make(map[string]string, len(imageNames))
	for _, name := range imageNames {
		data, err := os.ReadFile(filepath.Join(directory, "digests", "digest-"+name+".txt"))
		if err != nil {
			return err
		}
		digest := strings.TrimSpace(string(data))
		if !digestRE.MatchString(digest) {
			return fmt.Errorf("missing or invalid %s digest", name)
		}
		images[name] = imageRepository(name) + "@" + digest
	}
	archive := "orka-" + strings.TrimPrefix(version, "v") + ".tgz"
	digest, err := fileHash(filepath.Join(directory, archive))
	if err != nil {
		return err
	}
	cli, err := cliArtifacts(directory, version)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(directory, candidateFile), candidateBundle{
		SchemaVersion: 2, Repository: repository, Version: version, CandidateSHA: candidate,
		Branch: branch, BuildRunID: w.env("GITHUB_RUN_ID"), BuildRunAttempt: attempt,
		Images: images, Chart: releaseArtifact{File: archive, SHA256: digest}, CLI: cli,
	})
}

func loadBundle(directory string) (candidateBundle, error) {
	var data candidateBundle
	if err := readJSON(filepath.Join(directory, candidateFile), &data); err != nil {
		return data, err
	}
	if data.SchemaVersion != 2 || data.Repository != repository {
		return data, errors.New("invalid release bundle")
	}
	if !shaRE.MatchString(data.CandidateSHA) {
		return data, errors.New("invalid bundle candidate")
	}
	branch, err := branchFor(data.Version)
	if err != nil {
		return data, err
	}
	if data.Branch != branch {
		return data, errors.New("invalid bundle branch")
	}
	if !positiveNumber(data.BuildRunID) || !positiveNumber(data.BuildRunAttempt) {
		return data, errors.New("invalid bundle run identity")
	}
	if len(data.Images) != len(imageNames) {
		return data, errors.New("release bundle must contain every release image")
	}
	for _, name := range imageNames {
		prefix := imageRepository(name) + "@"
		ref := data.Images[name]
		if !strings.HasPrefix(ref, prefix) || !digestRE.MatchString(strings.TrimPrefix(ref, prefix)) {
			return data, fmt.Errorf("invalid release image reference for %s", name)
		}
	}
	if data.Chart.File != "orka-"+strings.TrimPrefix(data.Version, "v")+".tgz" {
		return data, errors.New("invalid chart archive path")
	}
	err = verifyHash(filepath.Join(directory, data.Chart.File), data.Chart.SHA256, "candidate chart bytes changed")
	if err != nil {
		return data, err
	}
	cli, err := cliArtifacts(directory, data.Version)
	if err != nil {
		return data, err
	}
	if !slices.Equal(data.CLI, cli) {
		return data, errors.New("candidate CLI assets changed or are incomplete")
	}
	return data, nil
}

func (w *workflow) downloadBundle(runID, attempt, branch, candidate, directory string) error {
	if !positiveNumber(runID) || !positiveNumber(attempt) {
		return errors.New("invalid build run")
	}
	var run workflowRun
	endpoint := repoAPI + "/actions/runs/" + runID
	if err := w.api(endpoint, http.MethodGet, nil, &run); err != nil {
		return err
	}
	if err := validateRun(run, releaseWorkflow, branch, candidate); err != nil {
		return err
	}
	wantedAttempt, err := strconv.ParseInt(attempt, 10, 64)
	if err != nil || run.Status != "in_progress" || run.RunAttempt < wantedAttempt {
		return errors.New("release build must belong to the active candidate run")
	}
	name := "release-candidate-" + runID + "-" + attempt
	artifacts, err := paginated[runArtifact](w, endpoint+"/artifacts?per_page=100", "artifacts")
	if err != nil {
		return err
	}
	count := 0
	for _, artifact := range artifacts {
		if artifact.Name == name && artifact.Expired != nil && !*artifact.Expired {
			count++
		}
	}
	if count != 1 {
		return errors.New("missing or ambiguous candidate artifact")
	}
	if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return err
	}
	_, err = w.command("gh", "run", "download", runID, "--repo", repository, "--name", name, "--dir", directory)
	if err != nil {
		return err
	}
	data, err := loadBundle(directory)
	if err != nil {
		return err
	}
	if data.BuildRunID != runID || data.BuildRunAttempt != attempt ||
		data.CandidateSHA != candidate || data.Branch != branch {
		return errors.New("candidate artifact does not match build run")
	}
	var after workflowRun
	if err := w.api(endpoint, http.MethodGet, nil, &after); err != nil {
		return err
	}
	if err := validateRun(after, releaseWorkflow, branch, candidate); err != nil {
		return err
	}
	if after.RunAttempt != run.RunAttempt || after.Status != "in_progress" {
		return errors.New("candidate run changed while downloading artifacts")
	}
	return nil
}

func verifyReport(directory string, report acceptanceReport) error {
	data, err := loadBundle(directory)
	if err != nil {
		return err
	}
	if report.Result != "qualified" || report.CandidateSHA != data.CandidateSHA {
		return errors.New("report does not qualify this candidate")
	}
	digest, err := fileHash(filepath.Join(directory, candidateFile))
	if err != nil {
		return err
	}
	expected := map[string]string{"buildRunID": data.BuildRunID, "buildRunAttempt": data.BuildRunAttempt,
		"bundleSHA256": digest, "version": data.Version}
	if !maps.Equal(report.Release, expected) {
		return errors.New("qualification used different release artifacts")
	}
	for role, name := range imageRoles {
		if report.BuiltImages[role] != data.Images[name] || report.Images[role].RequestedImage != data.Images[name] {
			return errors.New("qualification images differ from publication images")
		}
	}
	chart := report.Chart
	if chart.PackageSHA256 != data.Chart.SHA256 || !chart.Install || !chart.ContainerTask ||
		!chart.Recovery || !chart.NoReplay || !chart.OppositeModeRejected {
		return errors.New("release chart installation and recovery are not qualified")
	}
	return nil
}

func (w *workflow) qualify(directory string) error {
	if err := w.checkRunIdentity(); err != nil {
		return err
	}
	data, err := loadBundle(directory)
	if err != nil {
		return err
	}
	if err := w.checkContext(data.Branch, data.CandidateSHA); err != nil {
		return err
	}
	if data.BuildRunID != w.env("GITHUB_RUN_ID") {
		return errors.New("qualification must use this workflow's own candidate bundle")
	}
	endpoint := repoAPI + "/actions/workflows/" + qualificationWorkflow + "/runs?per_page=100"
	runs, err := paginated[workflowRun](w, endpoint, "workflow_runs")
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status != runCompleted {
			return errors.New("another release qualification run is active or awaiting approval; " +
				"finish or cancel it before retrying")
		}
	}
	run, err := w.dispatch(qualificationWorkflow, data.Branch, data.CandidateSHA, map[string]string{
		"source_repository": "https://github.com/" + repository + ".git",
		"source_ref":        data.CandidateSHA, "pr_base": data.Branch,
		"release_run_id": data.BuildRunID, "release_run_attempt": data.BuildRunAttempt,
		"dispatch_id": "release-" + w.env("GITHUB_RUN_ID") + "-" + w.env("GITHUB_RUN_ATTEMPT"),
	}, true)
	if err != nil {
		return err
	}
	if _, err := w.command("bash", "scripts/verify-release-qualification.sh", data.CandidateSHA,
		strconv.FormatInt(run.ID, 10), data.Branch); err != nil {
		return err
	}
	source := qualificationPath(w.root, run.ID, run.RunAttempt)
	var report acceptanceReport
	if err := readJSON(source, &report); err != nil {
		return err
	}
	if err := verifyReport(directory, report); err != nil {
		return err
	}
	if err := copyFile(source, filepath.Join(directory, acceptanceFile)); err != nil {
		return err
	}
	acceptanceHash, err := fileHash(source)
	if err != nil {
		return err
	}
	candidateHash, err := fileHash(filepath.Join(directory, candidateFile))
	if err != nil {
		return err
	}
	proof := qualificationProof{RunID: strconv.FormatInt(run.ID, 10), RunAttempt: strconv.FormatInt(run.RunAttempt, 10),
		AcceptanceSHA256: acceptanceHash, CandidateSHA256: candidateHash}
	if err := writeJSON(filepath.Join(directory, qualificationFile), proof); err != nil {
		return err
	}
	return w.summary(fmt.Sprintf("Qualified `%s` with the packaged chart and published image digests.\n\n"+
		"[Release qualification and cleanup evidence](%s).\n\n"+
		"Review the candidate artifact and generated commit before approving the release job.",
		data.CandidateSHA, run.HTMLURL))
}

func (w *workflow) verifyPublication(directory string) (candidateBundle, error) {
	data, err := loadBundle(directory)
	if err != nil {
		return data, err
	}
	if err := w.checkContext(data.Branch, data.CandidateSHA); err != nil {
		return data, err
	}
	if data.BuildRunID != w.env("GITHUB_RUN_ID") {
		return data, errors.New("publication must run in the workflow that built this candidate")
	}
	if err := w.checkEnvironment("release", data.Branch); err != nil {
		return data, err
	}
	if err := w.verifyProof(data, directory); err != nil {
		return data, err
	}
	// A rerun cannot replace images, chart, or CLI assets while earlier evidence awaits approval.
	temporary, err := os.MkdirTemp("", "orka-release-candidate-")
	if err != nil {
		return data, err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	original := filepath.Join(temporary, "candidate")
	err = w.downloadBundle(data.BuildRunID, data.BuildRunAttempt, data.Branch, data.CandidateSHA, original)
	if err != nil {
		return data, err
	}
	digest, err := fileHash(filepath.Join(directory, candidateFile))
	if err != nil {
		return data, err
	}
	err = verifyHash(filepath.Join(original, candidateFile), digest, "build artifact changed after qualification")
	if err != nil {
		return data, err
	}
	return data, w.verifyCLISignature(data, directory)
}

func (w *workflow) verifyProof(data candidateBundle, directory string) error {
	var proof qualificationProof
	if err := readJSON(filepath.Join(directory, qualificationFile), &proof); err != nil {
		return err
	}
	hashes := map[string]string{candidateFile: proof.CandidateSHA256, acceptanceFile: proof.AcceptanceSHA256}
	for name, digest := range hashes {
		if err := verifyHash(filepath.Join(directory, name), digest, "approved qualification artifacts changed"); err != nil {
			return err
		}
	}
	if !positiveNumber(proof.RunID) || !positiveNumber(proof.RunAttempt) {
		return errors.New("invalid qualification run identity")
	}
	_, err := w.command("bash", "scripts/verify-release-qualification.sh", data.CandidateSHA, proof.RunID, data.Branch)
	if err != nil {
		return err
	}
	var current workflowRun
	if err := w.api(repoAPI+"/actions/runs/"+proof.RunID, http.MethodGet, nil, &current); err != nil {
		return err
	}
	if strconv.FormatInt(current.RunAttempt, 10) != proof.RunAttempt {
		return errors.New("qualification was rerun after approval evidence was prepared")
	}
	reportPath := filepath.Join(w.root, "bin", "release-qualification-"+proof.RunID+"-"+proof.RunAttempt, acceptanceFile)
	if err := verifyHash(reportPath, proof.AcceptanceSHA256, "qualified report changed"); err != nil {
		return err
	}
	var report acceptanceReport
	if err := readJSON(reportPath, &report); err != nil {
		return err
	}
	return verifyReport(directory, report)
}
