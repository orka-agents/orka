package publisher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const gitObjectFormatSHA1 = "sha1"

var hardeningConfig = []string{
	"credential.helper=",
	"credential.interactive=never",
	"core.askPass=",
	"core.hooksPath=/dev/null",
	"core.attributesFile=/dev/null",
	"core.excludesFile=/dev/null",
	"core.fsmonitor=false",
	"core.untrackedCache=false",
	"commit.gpgSign=false",
	"tag.gpgSign=false",
	"protocol.allow=never",
	"protocol.file.allow=always",
	"protocol.https.allow=always",
	"protocol.ssh.allow=always",
	"protocol.ext.allow=never",
	"submodule.recurse=false",
	"fetch.recurseSubmodules=false",
	"push.recurseSubmodules=no",
	"filter.lfs.required=false",
	"filter.lfs.clean=",
	"filter.lfs.smudge=",
	"filter.lfs.process=",
	"fetch.fsckObjects=true",
	"transfer.fsckObjects=true",
	"receive.fsckObjects=true",
	"gc.auto=0",
	"maintenance.auto=false",
	"http.followRedirects=false",
}

type sandbox struct {
	root     string
	home     string
	xdg      string
	template string
}

func (p *Publisher) newSandbox(prefix string) (*sandbox, error) {
	root, err := os.MkdirTemp(p.tempRoot, "orka-publisher-"+prefix+"-")
	if err != nil {
		return nil, fmt.Errorf("create publisher sandbox: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("restrict publisher sandbox: %w", err)
	}
	result := &sandbox{root: root, home: filepath.Join(root, "home"), xdg: filepath.Join(root, "xdg"), template: filepath.Join(root, "template")}
	if err := os.Mkdir(result.home, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	if err := os.Mkdir(result.xdg, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	if err := os.Mkdir(result.template, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return result, nil
}

func (s *sandbox) Close() error {
	if s == nil || s.root == "" {
		return nil
	}
	return os.RemoveAll(s.root)
}

type commandResult struct {
	stdout    string
	stderr    string
	truncated bool
}

func (p *Publisher) runGit(ctx context.Context, box *sandbox, directory string, extraEnv map[string]string, stdin []byte, args ...string) (commandResult, error) {
	fullArgs := make([]string, 0, len(hardeningConfig)*2+len(args))
	for _, setting := range hardeningConfig {
		fullArgs = append(fullArgs, "-c", setting)
	}
	fullArgs = append(fullArgs, args...)
	if p.commandRecord != nil {
		p.commandRecord(append([]string(nil), fullArgs...))
	}
	command := exec.CommandContext(ctx, p.gitBinary, fullArgs...)
	configureCommandCancellation(command)
	command.Dir = directory
	command.Env = p.commandEnvironment(box, extraEnv)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	stdout := &boundedCommandBuffer{limit: p.maxCommandOutput}
	stderr := &boundedCommandBuffer{limit: p.maxCommandOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), truncated: stdout.truncated || stderr.truncated}
	if err != nil {
		if result.truncated {
			return result, fmt.Errorf("git output exceeded %d bytes: %w", p.maxCommandOutput, err)
		}
		message := strings.TrimSpace(result.stderr)
		if message == "" {
			message = strings.TrimSpace(result.stdout)
		}
		if len(message) > 0 {
			return result, fmt.Errorf("git %s: %s: %w", commandName(args), message, err)
		}
		return result, fmt.Errorf("git %s: %w", commandName(args), err)
	}
	if result.truncated {
		return result, fmt.Errorf("git output exceeded %d bytes", p.maxCommandOutput)
	}
	return result, nil
}

func commandName(args []string) string {
	for _, current := range args {
		if current == "" || strings.HasPrefix(current, "-") {
			continue
		}
		return current
	}
	return "command"
}

func (p *Publisher) commandEnvironment(box *sandbox, extra map[string]string) []string {
	values := make(map[string]string, len(extra)+16)
	maps.Copy(values, extra)
	hardened := map[string]string{
		"HOME":                   box.home,
		"XDG_CONFIG_HOME":        box.xdg,
		"GIT_CONFIG_GLOBAL":      "/dev/null",
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_TEMPLATE_DIR":       box.template,
		"GIT_NO_REPLACE_OBJECTS": "1",
		"GIT_EXEC_PATH":          p.gitExecPath,
		"GIT_TERMINAL_PROMPT":    "0",
		"GIT_ASKPASS":            "/bin/false",
		"SSH_ASKPASS":            "/bin/false",
		"GIT_ALLOW_PROTOCOL":     "file:https:ssh",
		"GIT_PROTOCOL_FROM_USER": "0",
		"GIT_OPTIONAL_LOCKS":     "0",
		"LC_ALL":                 "C",
		"LANG":                   "C",
		"TZ":                     "UTC",
		"PATH":                   p.trustedPath,
	}
	maps.Copy(values, hardened)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	environment = append(environment, p.proxyEnvironment.Variables()...)
	return environment
}

type boundedCommandBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	truncated bool
}

func (b *boundedCommandBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		if int64(len(value)) > remaining {
			value = value[:remaining]
			b.truncated = true
		}
		_, _ = b.buffer.Write(value)
	} else if len(value) > 0 {
		b.truncated = true
	}
	return original, nil
}

func (b *boundedCommandBuffer) String() string { return b.buffer.String() }

func objectFormat(oid string) string {
	if len(oid) == 64 {
		return "sha256"
	}
	return gitObjectFormatSHA1
}

func (p *Publisher) initBare(ctx context.Context, box *sandbox, repositoryPath, oid string) error {
	_, err := p.runGit(ctx, box, box.root, nil, nil, "init", "--bare", "--object-format="+objectFormat(oid), "--", repositoryPath)
	return err
}

func (p *Publisher) observeSource(ctx context.Context, box *sandbox, repository Repository, ref string) (RemoteRef, error) {
	if validateObjectID("source ref", ref) != nil {
		return p.observeRef(ctx, box, repository, ref)
	}
	if p.observeFault != nil {
		if err := p.observeFault(ctx, repository, ref); err != nil {
			return RemoteRef{}, err
		}
	}
	repositoryPath := filepath.Join(box.root, "source-observe.git")
	if err := p.initBare(ctx, box, repositoryPath, ref); err != nil {
		return RemoteRef{}, fmt.Errorf("initialize exact source observation: %w", err)
	}
	if _, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "fetch", "--no-tags", "--no-recurse-submodules", "--depth=1", "--", repository.URL, ref+":refs/orka/source"); err != nil {
		return RemoteRef{}, fmt.Errorf("fetch exact source commit: %w", err)
	}
	resolved, err := p.revParse(ctx, box, repositoryPath, "refs/orka/source^{commit}")
	if err != nil {
		return RemoteRef{}, err
	}
	if resolved != ref {
		return RemoteRef{}, operationError(ErrSourceMoved, "observe exact source commit", "fetched source did not match the requested commit", nil)
	}
	if err := p.fsckCommit(ctx, box, repositoryPath, resolved); err != nil {
		return RemoteRef{}, operationError(ErrPreparedArtifactCorrupt, "observe exact source commit", "strict fsck failed", err)
	}
	return RemoteRef{OID: resolved}, nil
}

func (p *Publisher) observeRef(ctx context.Context, box *sandbox, repository Repository, ref string) (RemoteRef, error) {
	if p.observeFault != nil {
		if err := p.observeFault(ctx, repository, ref); err != nil {
			return RemoteRef{}, err
		}
	}
	args := []string{"ls-remote", "--refs", "--", repository.URL, ref}
	peeledRef := ""
	if strings.HasPrefix(ref, "refs/tags/") {
		peeledRef = ref + "^{}"
		args = []string{"ls-remote", "--", repository.URL, ref, peeledRef}
	}
	result, err := p.runGit(ctx, box, box.root, nil, nil, args...)
	if err != nil {
		return RemoteRef{}, err
	}
	trimmed := strings.TrimSpace(result.stdout)
	if trimmed == "" {
		return RemoteRef{Absent: true}, nil
	}
	var directOID, peeledOID string
	for line := range strings.SplitSeq(trimmed, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return RemoteRef{}, fmt.Errorf("remote returned malformed exact ref observation")
		}
		if err := validateObjectID("observed remote", fields[0]); err != nil {
			return RemoteRef{}, err
		}
		switch fields[1] {
		case ref:
			if directOID != "" {
				return RemoteRef{}, fmt.Errorf("remote returned multiple rows for exact ref %q", ref)
			}
			directOID = fields[0]
		case peeledRef:
			if peeledRef == "" || peeledOID != "" {
				return RemoteRef{}, fmt.Errorf("remote returned malformed exact ref observation")
			}
			peeledOID = fields[0]
		default:
			return RemoteRef{}, fmt.Errorf("remote returned malformed exact ref observation")
		}
	}
	if directOID == "" {
		return RemoteRef{}, fmt.Errorf("remote returned malformed exact ref observation")
	}
	if peeledOID != "" {
		return RemoteRef{OID: peeledOID}, nil
	}
	return RemoteRef{OID: directOID}, nil
}

func (p *Publisher) importBundle(ctx context.Context, box *sandbox, prepared PreparedPublication) (string, error) {
	repositoryPath := filepath.Join(box.root, "objects.git")
	if err := p.initBare(ctx, box, repositoryPath, prepared.CommitOID); err != nil {
		return "", fmt.Errorf("initialize bundle repository: %w", err)
	}
	result, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "fetch", "--no-tags", "--no-recurse-submodules", "--", prepared.BundlePath, prepared.BundleRef+":refs/orka/prepared")
	if err != nil {
		return "", fmt.Errorf("import durable bundle: %w", err)
	}
	_ = result
	resolved, err := p.revParse(ctx, box, repositoryPath, "refs/orka/prepared^{commit}")
	if err != nil {
		return "", err
	}
	if resolved != prepared.CommitOID {
		return "", operationError(ErrPreparedArtifactCorrupt, "import durable bundle", "bundle ref does not resolve to persisted commit", nil)
	}
	if err := p.fsckCommit(ctx, box, repositoryPath, prepared.CommitOID); err != nil {
		return "", operationError(ErrPreparedArtifactCorrupt, "import durable bundle", "strict fsck failed", err)
	}
	return repositoryPath, nil
}

func (p *Publisher) revParse(ctx context.Context, box *sandbox, repositoryPath, revision string) (string, error) {
	result, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "rev-parse", "--verify", revision)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(result.stdout)
	if err := validateObjectID("resolved revision", value); err != nil {
		return "", err
	}
	return value, nil
}

func (p *Publisher) isAncestor(ctx context.Context, box *sandbox, repositoryPath, ancestor, descendant string) (bool, error) {
	_, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}
