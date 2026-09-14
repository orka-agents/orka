package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	gitCommand               = "git"
	gitDiff                  = "diff"
	gitCommit                = "commit"
	makeCommand              = "make"
	policyBranch             = "branch"
	runCompleted             = "completed"
	controllerImage          = "controller"
	publisherImage           = "workspace-publisher"
	makefilePath             = "Makefile"
	chartInputPath           = "cmd/build/helmify/static/Chart.yaml"
	valuesInputPath          = "cmd/build/helmify/static/values.yaml"
	repository               = "orka-agents/orka"
	repoAPI                  = "repos/" + repository
	releaseWorkflow          = "release.yml"
	qualificationWorkflow    = "release-qualification.yml"
	qualificationEnvironment = "release-qualification"
	candidateFile            = "candidate.json"
	qualificationFile        = "qualification.json"
	acceptanceFile           = "acceptance.json"
	pagesBranch              = "gh-pages"
	chartURL                 = "https://orka-agents.github.io/orka/charts"
	versionPattern           = `v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(beta|rc)\.(0|[1-9][0-9]*))?`
)

var (
	versionRE  = regexp.MustCompile("^" + versionPattern + "$")
	shaRE      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestRE   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	numberRE   = regexp.MustCompile(`^[0-9]+$`)
	branchRE   = regexp.MustCompile(`^release-(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	imageNames = []string{
		controllerImage, "ai-worker", "general-worker", "agent-harness-wrapper",
		"acp-codex-runtime", "acp-claude-runtime", "acp-copilot-runtime",
		"acp-opencode-runtime", publisherImage,
	}
	imageRoles = map[string]string{
		controllerImage: controllerImage, "publisher": publisherImage,
		"codex": "acp-codex-runtime", "claude": "acp-claude-runtime",
		"copilot": "acp-copilot-runtime", "opencode": "acp-opencode-runtime",
	}
	gitAuth = []string{gitCommand, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}
)

type commandSpec struct {
	dir    string
	args   []string
	input  string
	stream bool
}

type commandResult struct {
	stdout string
	stderr string
	err    error
}

// Only external effects are replaceable in tests. Release checks always execute.
type workflow struct {
	root   string
	run    func(commandSpec) commandResult
	get    func(string) ([]byte, error)
	env    func(string) string
	now    func() time.Time
	sleep  func(time.Duration)
	stdout io.Writer
}

func newWorkflow(root string) *workflow {
	return &workflow{
		root: root, run: runCommand, get: getURL, env: os.Getenv,
		now: time.Now, sleep: time.Sleep, stdout: os.Stdout,
	}
}

func runCommand(spec commandSpec) commandResult {
	cmd := exec.Command(spec.args[0], spec.args[1:]...)
	cmd.Dir = spec.dir
	cmd.Stdin = strings.NewReader(spec.input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if spec.stream {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	err := cmd.Run()
	return commandResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func getURL(address string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Cache-Control", "no-cache")
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("chart server returned an unsuccessful status")
	}
	return io.ReadAll(response.Body)
}

func (w *workflow) command(args ...string) (string, error) {
	value, err := w.commandRaw(w.root, "", args...)
	return strings.TrimSpace(value), err
}

func (w *workflow) commandRaw(dir, input string, args ...string) (string, error) {
	result := w.run(commandSpec{dir: dir, args: args, input: input})
	if result.err != nil {
		// Commands may handle credentials. Never include arguments or captured output.
		return "", fmt.Errorf("%s command failed", args[0])
	}
	return result.stdout, nil
}

func (w *workflow) api(path, method string, payload, target any) error {
	args := []string{"gh", "api", path}
	var input string
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		input = string(data)
		args = append(args, "--method", method, "--input", "-")
	}
	result, err := w.commandRaw(w.root, input, args...)
	if err != nil {
		return fmt.Errorf("GitHub API %s %s failed; check job token permissions and repository configuration", method, path)
	}
	if target == nil {
		return nil
	}
	return decodeJSON([]byte(result), target)
}

func paginated[T any](w *workflow, path, key string) ([]T, error) {
	output, err := w.command("gh", "api", "--paginate", "--slurp", path)
	if err != nil {
		return nil, err
	}
	var pages []json.RawMessage
	if err := decodeJSON([]byte(output), &pages); err != nil {
		return nil, err
	}
	var results []T
	for _, page := range pages {
		if key != "" {
			var object map[string]json.RawMessage
			if err := decodeJSON(page, &object); err != nil {
				return nil, err
			}
			page = object[key]
		}
		var entries []T
		if err := decodeJSON(page, &entries); err != nil {
			return nil, err
		}
		results = append(results, entries...)
	}
	return results, nil
}

func decodeJSON(data []byte, target any) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("missing JSON response or release artifact")
	}
	if err := json.Unmarshal(data, target); err != nil {
		// JSON errors can contain snippets of credential-bearing command responses.
		return errors.New("invalid JSON response or release artifact")
	}
	return nil
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeJSON(data, target)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return hashBytes(data), nil
}

func verifyHash(path, expected, message string) error {
	actual, err := fileHash(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New(message)
	}
	return nil
}

func copyFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0o600)
}

func branchFor(version string) (string, error) {
	match := versionRE.FindStringSubmatch(version)
	if match == nil {
		return "", errors.New("version must be vX.Y.Z[-beta.N|-rc.N] without leading zeros")
	}
	return "release-" + match[1] + "." + match[2], nil
}

func positiveNumber(value string) bool {
	return numberRE.MatchString(value) && strings.Trim(value, "0") != ""
}

func imageRepository(name string) string {
	if name == controllerImage {
		return "ghcr.io/" + repository
	}
	return "ghcr.io/" + repository + "/" + name
}

func appendFile(path, text string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, text)
	return errors.Join(writeErr, file.Close())
}

func (w *workflow) output(name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("invalid workflow output")
	}
	line := name + "=" + value + "\n"
	if path := w.env("GITHUB_OUTPUT"); path != "" {
		if err := appendFile(path, line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w.stdout, line)
	return err
}

func (w *workflow) summary(text string) error {
	if path := w.env("GITHUB_STEP_SUMMARY"); path != "" {
		return appendFile(path, text+"\n")
	}
	return nil
}

type gitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type gitRef struct {
	Ref    string    `json:"ref"`
	Object gitObject `json:"object"`
}

type workflowRun struct {
	ID             int64           `json:"id"`
	RunAttempt     int64           `json:"run_attempt"`
	Status         string          `json:"status"`
	Conclusion     string          `json:"conclusion"`
	Event          string          `json:"event"`
	Path           string          `json:"path"`
	HeadSHA        string          `json:"head_sha"`
	HeadBranch     string          `json:"head_branch"`
	Repository     namedRepository `json:"repository"`
	HeadRepository namedRepository `json:"head_repository"`
	DisplayTitle   string          `json:"display_title"`
	HTMLURL        string          `json:"html_url"`
}

type namedRepository struct {
	FullName string `json:"full_name"`
}

func (w *workflow) branchHead(branch string) (string, error) {
	var ref gitRef
	err := w.api(repoAPI+"/git/ref/heads/"+url.PathEscape(branch), http.MethodGet, nil, &ref)
	return ref.Object.SHA, err
}

func (w *workflow) defaultBranch() (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := w.api(repoAPI, http.MethodGet, nil, &repo); err != nil {
		return "", err
	}
	if repo.DefaultBranch == "" {
		return "", errors.New("repository default branch is missing")
	}
	return repo.DefaultBranch, nil
}

func (w *workflow) checkContext(branch, candidate string) error {
	if !shaRE.MatchString(candidate) || w.env("GITHUB_REPOSITORY") != repository ||
		w.env("GITHUB_EVENT_NAME") != "workflow_dispatch" || w.env("GITHUB_REF") != "refs/heads/"+branch ||
		w.env("GITHUB_SHA") != candidate {
		return errors.New("release must run in orka-agents/orka from the candidate branch and dispatched workflow SHA")
	}
	head, err := w.command(gitCommand, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != candidate {
		return errors.New("checkout differs from candidate")
	}
	head, err = w.branchHead(branch)
	if err != nil {
		return err
	}
	if head != candidate {
		return errors.New("candidate branch moved; prepare and qualify its new head")
	}
	return nil
}

func validateRun(run workflowRun, workflowName, branch, candidate string) error {
	if run.ID <= 0 || run.RunAttempt <= 0 || run.Event != "workflow_dispatch" ||
		run.Path != ".github/workflows/"+workflowName ||
		run.HeadBranch != branch || run.HeadSHA != candidate || run.Repository.FullName != repository ||
		run.HeadRepository.FullName != repository {
		return errors.New("workflow run does not match the trusted candidate")
	}
	return nil
}

func (w *workflow) tagRef(version string) (*gitRef, error) {
	var refs []gitRef
	if err := w.api(repoAPI+"/git/matching-refs/tags/"+version, http.MethodGet, nil, &refs); err != nil {
		return nil, err
	}
	var match *gitRef
	for _, ref := range refs {
		if ref.Ref == "refs/tags/"+version {
			if match != nil {
				return nil, errors.New("ambiguous release tag")
			}
			match = &ref
		}
	}
	return match, nil
}

func runPath(id int64) string { return repoAPI + "/actions/runs/" + strconv.FormatInt(id, 10) }

func (w *workflow) dispatch(name, branch, candidate string, inputs map[string]string, wait bool) (workflowRun, error) {
	head, err := w.branchHead(branch)
	if err != nil {
		return workflowRun{}, err
	}
	if head != candidate {
		return workflowRun{}, errors.New("branch moved before dispatch")
	}
	endpoint := repoAPI + "/actions/workflows/" + name + "/runs?event=workflow_dispatch&branch=" +
		url.QueryEscape(branch) + "&per_page=100"
	var list struct {
		Runs []workflowRun `json:"workflow_runs"`
	}
	if err := w.api(endpoint, http.MethodGet, nil, &list); err != nil {
		return workflowRun{}, err
	}
	previous := make(map[int64]bool, len(list.Runs))
	for _, run := range list.Runs {
		previous[run.ID] = true
	}
	payload := map[string]any{"ref": branch, "inputs": inputs}
	if err := w.api(repoAPI+"/actions/workflows/"+name+"/dispatches", http.MethodPost, payload, nil); err != nil {
		return workflowRun{}, err
	}
	run, err := w.observeDispatch(endpoint, previous, name, branch, candidate, inputs["dispatch_id"])
	if err != nil {
		return workflowRun{}, err
	}
	if _, err := fmt.Fprintf(w.stdout, "Started https://github.com/%s/actions/runs/%d\n", repository, run.ID); err != nil {
		return workflowRun{}, err
	}
	if wait {
		return w.waitQualification(run, name, branch, candidate)
	}
	return run, nil
}

func (w *workflow) observeDispatch(
	endpoint string, previous map[int64]bool, name, branch, candidate, dispatchID string,
) (workflowRun, error) {
	deadline := w.now().Add(120 * time.Second)
	for {
		var list struct {
			Runs []workflowRun `json:"workflow_runs"`
		}
		if err := w.api(endpoint, http.MethodGet, nil, &list); err != nil {
			return workflowRun{}, err
		}
		var match *workflowRun
		for _, run := range list.Runs {
			if previous[run.ID] || run.HeadSHA != candidate || !strings.Contains(run.DisplayTitle, dispatchID) {
				continue
			}
			if match != nil {
				return workflowRun{}, errors.New("dispatch matched multiple workflow runs")
			}
			match = &run
		}
		if match != nil {
			return *match, validateRun(*match, name, branch, candidate)
		}
		if !w.now().Before(deadline) {
			return workflowRun{}, errors.New("dispatched workflow was not observed; inspect Actions before retrying")
		}
		w.sleep(5 * time.Second)
	}
}

func (w *workflow) waitQualification(run workflowRun, name, branch, candidate string) (workflowRun, error) {
	// Reserve time for evidence within the parent's six-hour runner limit.
	deadline := w.now().Add(5*time.Hour + 40*time.Minute)
	var approvalDeadline time.Time
	for run.Status != runCompleted {
		now := w.now()
		if run.Status == "waiting" && approvalDeadline.IsZero() {
			approvalDeadline = now.Add(90 * time.Minute)
		}
		var reason string
		nextStep := "publication is blocked"
		// Pending indicates concurrency contention, unlike approval waiting or a queued runner.
		switch {
		case run.Status == "pending":
			reason = "qualification queued behind another run"
			nextStep = "finish the active run and retry"
		case !now.Before(deadline):
			reason = "qualification timed out"
		case run.Status == "waiting" && !now.Before(approvalDeadline):
			reason = "qualification approval timed out after 90 minutes"
		}
		if reason != "" {
			if err := w.api(runPath(run.ID)+"/cancel", http.MethodPost, map[string]any{}, nil); err != nil {
				return workflowRun{}, fmt.Errorf("%s; cancellation request failed: %w", reason, err)
			}
			return workflowRun{}, fmt.Errorf("%s; cancellation requested; %s", reason, nextStep)
		}
		w.sleep(15 * time.Second)
		var next workflowRun
		if err := w.api(runPath(run.ID), http.MethodGet, nil, &next); err != nil {
			return workflowRun{}, err
		}
		if err := validateRun(next, name, branch, candidate); err != nil {
			return workflowRun{}, err
		}
		run = next
	}
	if run.Conclusion != "success" {
		return workflowRun{}, errors.New("qualification failed; inspect the linked run")
	}
	return run, nil
}

func qualificationPath(root string, id, attempt int64) string {
	return filepath.Join(root, "bin", fmt.Sprintf("release-qualification-%d-%d", id, attempt), acceptanceFile)
}
