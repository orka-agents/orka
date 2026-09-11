package publisher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxDeltaBytes  = int64(256 << 20)
	defaultMaxBundleBytes = int64(512 << 20)
	defaultCommandOutput  = int64(1 << 20)
	defaultPublishTimeout = 2 * time.Minute
	defaultVerifyAttempts = 3
	defaultVerifyBackoff  = 100 * time.Millisecond
)

// Options configures the clean-room publisher. ArtifactRoot is durable trusted
// storage; TempRoot may be ephemeral. Child process environments are always
// rebuilt from an empty allowlist.
type Options struct {
	ArtifactRoot     string
	TempRoot         string
	GitBinary        string
	MaxDeltaBytes    int64
	MaxBundleBytes   int64
	MaxCommandOutput int64
	PublishTimeout   time.Duration
	VerifyAttempts   int
	VerifyBackoff    time.Duration
	ProxyEnvironment ProxyEnvironment
}

// Publisher is safe for concurrent use within one controller process. The
// first release intentionally serializes local durable receipt updates; remote
// correctness still relies on exact ref CAS, not this mutex.
type Publisher struct {
	artifactRoot     string
	tempRoot         string
	gitBinary        string
	gitExecPath      string
	trustedPath      string
	maxDeltaBytes    int64
	maxBundleBytes   int64
	maxCommandOutput int64
	publishTimeout   time.Duration
	verifyAttempts   int
	verifyBackoff    time.Duration
	proxyEnvironment ProxyEnvironment

	mu sync.Mutex

	// Test-only fault boundaries. They remain unexported so production callers
	// cannot weaken the mutation protocol.
	beforeCAS     func(context.Context, PublishRequest) error
	afterPush     func(context.Context, PublishRequest) error
	observeFault  func(context.Context, Repository, string) error
	commandRecord func([]string)
}

func New(options Options) (*Publisher, error) {
	if !filepath.IsAbs(options.ArtifactRoot) || filepath.Clean(options.ArtifactRoot) != options.ArtifactRoot {
		return nil, invalid("artifact root", "must be an absolute clean path")
	}
	if options.TempRoot != "" && (!filepath.IsAbs(options.TempRoot) || filepath.Clean(options.TempRoot) != options.TempRoot) {
		return nil, invalid("temporary root", "must be an absolute clean path")
	}
	absoluteGit, err := resolveGitBinary(options.GitBinary)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absoluteGit)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return nil, invalid("git binary", "%q is not an executable regular file", absoluteGit)
	}
	if err := ensureTrustedDirectory(options.ArtifactRoot); err != nil {
		return nil, fmt.Errorf("prepare artifact root: %w", err)
	}
	if options.TempRoot != "" {
		if err := ensureTrustedDirectory(options.TempRoot); err != nil {
			return nil, fmt.Errorf("prepare temporary root: %w", err)
		}
	}
	trustedPath := filepath.Dir(absoluteGit) + string(os.PathListSeparator) + "/usr/local/bin:/usr/bin:/bin"
	gitExecPath, err := resolveGitExecPath(absoluteGit, trustedPath)
	if err != nil {
		return nil, err
	}
	publisher := &Publisher{
		artifactRoot:     options.ArtifactRoot,
		tempRoot:         options.TempRoot,
		gitBinary:        absoluteGit,
		gitExecPath:      gitExecPath,
		trustedPath:      trustedPath,
		maxDeltaBytes:    options.MaxDeltaBytes,
		maxBundleBytes:   options.MaxBundleBytes,
		maxCommandOutput: options.MaxCommandOutput,
		publishTimeout:   options.PublishTimeout,
		verifyAttempts:   options.VerifyAttempts,
		verifyBackoff:    options.VerifyBackoff,
		proxyEnvironment: options.ProxyEnvironment,
	}
	if publisher.maxDeltaBytes <= 0 {
		publisher.maxDeltaBytes = defaultMaxDeltaBytes
	}
	if publisher.maxBundleBytes <= 0 {
		publisher.maxBundleBytes = defaultMaxBundleBytes
	}
	if publisher.maxCommandOutput <= 0 {
		publisher.maxCommandOutput = defaultCommandOutput
	}
	if publisher.publishTimeout <= 0 {
		publisher.publishTimeout = defaultPublishTimeout
	}
	if publisher.verifyAttempts <= 0 {
		publisher.verifyAttempts = defaultVerifyAttempts
	}
	if publisher.verifyBackoff <= 0 {
		publisher.verifyBackoff = defaultVerifyBackoff
	}
	return publisher, nil
}

func resolveGitBinary(configured string) (string, error) {
	if configured != "" {
		if !filepath.IsAbs(configured) || filepath.Clean(configured) != configured {
			return "", invalid("git binary", "configured path must be absolute and clean")
		}
		return configured, nil
	}
	for _, candidate := range []string{"/opt/homebrew/bin/git", "/usr/local/bin/git", "/usr/bin/git", "/bin/git"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("locate trusted git binary: no reviewed absolute path exists")
}

func resolveGitExecPath(gitBinary, trustedPath string) (string, error) {
	command := exec.Command(gitBinary, "--exec-path")
	command.Env = []string{
		"HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C", "LANG=C", "PATH=" + trustedPath,
	}
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("resolve trusted Git exec path: %w", err)
	}
	resolved := strings.TrimSpace(string(output))
	if !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", invalid("Git exec path", "%q is not absolute and clean", resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", invalid("Git exec path", "%q is not a directory", resolved)
	}
	return resolved, nil
}

func ensureTrustedDirectory(directory string) error {
	directory = filepath.Clean(directory)
	if !filepath.IsAbs(directory) {
		return fmt.Errorf("%q is not an absolute directory", directory)
	}
	missing := make([]string, 0, 4)
	current := directory
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%q is not a real directory", current)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("no trusted parent exists for %q", directory)
		}
		current = parent
	}
	for _, child := range slices.Backward(missing) {
		if err := os.Mkdir(child, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := os.Chmod(child, 0o700); err != nil {
			return err
		}
		if err := syncPublisherDirectory(filepath.Dir(child)); err != nil {
			return fmt.Errorf("sync created directory %q: %w", child, err)
		}
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is not a real directory", directory)
	}
	return os.Chmod(directory, 0o700)
}

func syncPublisherDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck
	return file.Sync()
}
