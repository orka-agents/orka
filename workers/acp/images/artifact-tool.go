// Command artifact-tool performs the two deterministic supply-chain checks used
// by the ACP image definitions. It intentionally depends only on the Go
// standard library so it can run in pinned build stages without installing
// another package manager.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var allowedDownloadHosts = map[string]struct{}{
	"codeload.github.com":                  {},
	"deb.debian.org":                       {},
	"github.com":                           {},
	"raw.githubusercontent.com":            {},
	"registry.npmjs.org":                   {},
	"release-assets.githubusercontent.com": {},
	"snapshot.debian.org":                  {},
}

var allowedDownloadQueryHosts = map[string]struct{}{
	"release-assets.githubusercontent.com": {},
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "artifact-tool:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: artifact-tool <command> [arguments]")
	}
	switch args[0] {
	case "download":
		if len(args) != 5 {
			return errors.New("usage: artifact-tool download <https-url> <sha256> <output> <max-bytes>")
		}
		maxBytes, err := strconv.ParseInt(args[4], 10, 64)
		if err != nil || maxBytes <= 0 {
			return errors.New("max-bytes must be a positive integer")
		}
		return download(args[1], args[2], args[3], maxBytes)
	case "compare":
		if len(args) != 3 {
			return errors.New("usage: artifact-tool compare <left-directory> <right-directory>")
		}
		return compareTrees(args[1], args[2])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func download(rawURL, expectedHex, output string, maxBytes int64) error {
	expectedHex = strings.ToLower(strings.TrimSpace(expectedHex))
	expected, err := hex.DecodeString(expectedHex)
	if err != nil || len(expected) != sha256.Size || hex.EncodeToString(expected) != expectedHex {
		return errors.New("expected digest must be a lowercase SHA-256 hex string")
	}

	parsed, err := validatedURL(rawURL)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 15 * time.Minute,
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			_, err := validatedURL(request.URL.String())
			return err
		},
	}
	response, err := client.Get(parsed.String())
	if err != nil {
		return fmt.Errorf("download %s%s: %w", parsed.Hostname(), parsed.EscapedPath(), redactDownloadError(err))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s%s: HTTP %d", parsed.Hostname(), parsed.EscapedPath(), response.StatusCode)
	}
	if response.ContentLength > maxBytes {
		return fmt.Errorf("download content length %d exceeds %d bytes", response.ContentLength, maxBytes)
	}

	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(output), ".artifact-partial-*")
	if err != nil {
		return fmt.Errorf("create temporary artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary artifact: %w", err)
	}

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("write artifact: %w", err)
	}
	if written > maxBytes {
		return fmt.Errorf("download exceeded %d bytes", maxBytes)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close artifact: %w", err)
	}
	actual := hash.Sum(nil)
	if !equalBytes(actual, expected) {
		return fmt.Errorf("SHA-256 mismatch: expected %s, got %s", expectedHex, hex.EncodeToString(actual))
	}
	if err := os.Rename(temporaryPath, output); err != nil {
		return fmt.Errorf("publish artifact: %w", err)
	}
	keep = true
	fmt.Printf("verified %s%s: sha256:%s (%d bytes)\n", parsed.Hostname(), parsed.EscapedPath(), expectedHex, written)
	return nil
}

func redactDownloadError(err error) error {
	var requestErr *url.Error
	if !errors.As(err, &requestErr) {
		return err
	}
	redacted := *requestErr
	redacted.Err = errors.New("request failed")
	parsed, parseErr := url.Parse(redacted.URL)
	if parseErr != nil {
		redacted.URL = ""
	} else {
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		redacted.URL = parsed.String()
	}
	return &redacted
}

func validatedURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse download URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("download URL must be credential-free HTTPS without a fragment")
	}
	host := parsed.Hostname()
	if _, ok := allowedDownloadHosts[host]; !ok {
		return nil, fmt.Errorf("download host is not allowlisted: %s", host)
	}
	if parsed.RawQuery != "" {
		if _, ok := allowedDownloadQueryHosts[host]; !ok {
			return nil, errors.New("download URL query is not allowed for this host")
		}
	}
	if parsed.RawPath != "" || path.Clean(parsed.Path) != parsed.Path || strings.Contains(parsed.Path, `\`) {
		return nil, errors.New("download URL path must be unescaped and canonical")
	}
	switch host {
	case "github.com":
		if !strings.HasPrefix(parsed.EscapedPath(), "/github/copilot-cli/releases/download/") {
			return nil, errors.New("GitHub download path is not allowlisted")
		}
	case "release-assets.githubusercontent.com":
		if !strings.HasPrefix(parsed.EscapedPath(), "/github-production-release-asset/") {
			return nil, errors.New("GitHub release asset path is not allowlisted")
		}
	case "deb.debian.org":
		if !strings.HasPrefix(parsed.EscapedPath(), "/debian/pool/main/r/rust-ripgrep/ripgrep_") ||
			!strings.HasSuffix(parsed.EscapedPath(), ".deb") {
			return nil, errors.New("debian ripgrep package path is not allowlisted")
		}
	case "snapshot.debian.org":
		fileID := strings.TrimPrefix(parsed.EscapedPath(), "/file/")
		decoded, decodeErr := hex.DecodeString(fileID)
		if decodeErr != nil || len(decoded) != 20 || parsed.EscapedPath() != "/file/"+fileID {
			return nil, errors.New("debian snapshot file path is not allowlisted")
		}
	}
	return parsed, nil
}

func compareTrees(leftRoot, rightRoot string) error {
	left, err := treeDigests(leftRoot)
	if err != nil {
		return fmt.Errorf("left tree: %w", err)
	}
	right, err := treeDigests(rightRoot)
	if err != nil {
		return fmt.Errorf("right tree: %w", err)
	}
	leftNames := sortedKeys(left)
	rightNames := sortedKeys(right)
	if strings.Join(leftNames, "\x00") != strings.Join(rightNames, "\x00") {
		return fmt.Errorf("tree file lists differ: left=%q right=%q", leftNames, rightNames)
	}
	for _, name := range leftNames {
		if left[name] != right[name] {
			return fmt.Errorf("file differs from published artifact: %s", name)
		}
	}
	fmt.Printf("matched %d files in %s and %s\n", len(leftNames), leftRoot, rightRoot)
	return nil
}

func treeDigests(root string) (map[string]string, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("comparison root must be a real directory")
	}
	result := map[string]string{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symbolic link: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file: %s", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(relative)] = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	return result, err
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}
