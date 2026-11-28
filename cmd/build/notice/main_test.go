package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoCoordinate(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"github.com/alecthomas/chroma/v2": "go/golang/github.com%2falecthomas%2fchroma/v2/v1.0.0",
		"cel.dev/expr":                    "go/golang/cel.dev/expr/v1.0.0",
		"example":                         "go/golang/-/example/v1.0.0",
	} {
		if got := goCoordinate(path, "v1.0.0"); got != want {
			t.Errorf("goCoordinate(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestUIPackagesFollowsProductionDependencies(t *testing.T) {
	t.Parallel()
	lockfile := filepath.Join(t.TempDir(), "bun.lock")
	// Trailing commas match bun's JSONC lockfile format.
	lock := `{
  "workspaces": {
    "": {
      "dependencies": { "@scope/app": "1", "react": "19", },
      "devDependencies": { "tailwindcss": "4", "vite": "6", },
    },
  },
  "packages": {
    "@scope/app": ["@scope/app@1.0.0", "", {
      "dependencies": { "shared": "2" },
      "peerDependencies": { "react": "19", "@types/react": "*" },
      "optionalPeers": ["@types/react"],
      "optionalDependencies": { "native": "1" },
    }, "sha512-a"],
    "@scope/app/shared": ["shared@2.0.0", "", {}, "sha512-b"],
    "shared": ["shared@1.0.0", "", {}, "sha512-c"],
    "react": ["react@19.0.0", "", {}, "sha512-d"],
    "@types/react": ["@types/react@19.0.0", "", {}, "sha512-e"],
    "native": ["native@1.0.0", "", {}, "sha512-f"],
    "tailwindcss": ["tailwindcss@4.1.18", "", {}, "sha512-h"],
    "vite": ["vite@6.0.0", "", { "dependencies": { "shared": "1" } }, "sha512-g"],
  },
}`
	if err := os.WriteFile(lockfile, []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := uiPackages(lockfile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"npm/npmjs/-/react/19.0.0",
		"npm/npmjs/-/shared/2.0.0",
		"npm/npmjs/-/tailwindcss/4.1.18",
		"npm/npmjs/@scope/app/1.0.0",
	}
	if got = dedupe(got); !slices.Equal(got, want) {
		t.Fatalf("uiPackages() = %q, want %q", got, want)
	}
}

func TestUIPackagesRejectsUnlockedDependency(t *testing.T) {
	t.Parallel()
	lockfile := filepath.Join(t.TempDir(), "bun.lock")
	lock := `{"workspaces": {"": {"dependencies": {"missing": "1"}}}, "packages": {}}`
	if err := os.WriteFile(lockfile, []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := uiPackages(lockfile); err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("uiPackages() error = %v, want unlocked dependency error", err)
	}
}

func TestDockerfileCoordinates(t *testing.T) {
	t.Parallel()
	dockerfile := `
RUN fetch "https://registry.npmjs.org/@openai/codex/-/codex-0.160.0-linux-x64.tgz" && \
    fetch "https://registry.npmjs.org/opencode-linux-arm64/-/opencode-linux-arm64-1.19.0.tgz" && \
    fetch "https://codeload.github.com/anomalyco/opencode/tar.gz/733b2c627e5aec39cb33cc22373bdce33053ef2c" && \
    fetch "https://deb.debian.org/debian/pool/main/r/rust-ripgrep/ripgrep_15.2.0-1_amd64.deb" && \
    fetch "https://github.com/github/copilot-cli/releases/download/v1.0.96/copilot-linux-x64.tar.gz"
`
	want := []string{
		"npm/npmjs/@openai/codex/0.160.0-linux-x64",
		"npm/npmjs/-/opencode-linux-arm64/1.19.0",
		"git/github/anomalyco/opencode/733b2c627e5aec39cb33cc22373bdce33053ef2c",
		"deb/debian/-/ripgrep/15.2.0-1_amd64",
	}
	if got := dockerfileCoordinates(dockerfile); !slices.Equal(got, want) {
		t.Fatalf("dockerfileCoordinates() = %q, want %q", got, want)
	}
}

func TestImageDownloadsSkipHandWrittenComponents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "workers", "acp", "images", "claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dockerfile := `RUN fetch https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.3.293.tgz \
    https://registry.npmjs.org/@agentclientprotocol/claude-agent-acp/-/claude-agent-acp-0.89.1.tgz`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := imageDownloads(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"npm/npmjs/@agentclientprotocol/claude-agent-acp/0.89.1"}; !slices.Equal(got, want) {
		t.Fatalf("imageDownloads() = %q, want %q", got, want)
	}
}

func TestFetchNoticesRetriesServerErrorsAndRendersStableOutput(t *testing.T) {
	retryDelay = func(int) time.Duration { return 0 }
	var (
		mu    sync.Mutex
		slept []time.Duration
	)
	sleep = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
	}
	t.Cleanup(func() { sleep = time.Sleep })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/notices" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		switch calls.Add(1) {
		case 1:
			http.Error(w, "warming", http.StatusBadGateway)
			return
		case 2:
			w.Header().Set("Retry-After", "7")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		var request struct {
			Coordinates []string `json:"coordinates"`
			Renderer    string   `json:"renderer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Renderer != "json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var response noticeResponse
		for _, coordinate := range request.Coordinates {
			switch coordinate {
			case "go/golang/github.com%2fexample/b/v1.0.0":
				response.Content.Packages = append(response.Content.Packages, noticePackage{
					Name: "github.com%2Fexample/b", Version: "v1.0.0", License: "MIT",
					Copyrights: []string{"Copyright 2 B", "Copyright 1 B", "Copyright 1 B"}, Text: "MIT text\r\n",
				})
			case "npm/npmjs/-/a/1.0.0":
				response.Content.Packages = append(response.Content.Packages,
					noticePackage{Name: "a", Version: "1.0.0", License: "ISC", Text: "ISC"})
			default:
				response.Summary.Warnings.NoDefinition = append(response.Summary.Warnings.NoDefinition, coordinate)
			}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	result, err := fetchNotices(server.Client(), server.URL,
		[]string{"go/golang/github.com%2fexample/b/v1.0.0", "npm/npmjs/-/a/1.0.0", "npm/npmjs/-/unknown/1.0.0"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Batches run concurrently, so the retry order is not fixed.
	slices.Sort(slept)
	if want := []time.Duration{0, 7 * time.Second}; !slices.Equal(slept, want) {
		t.Errorf("retry sleeps = %v, want %v", slept, want)
	}
	got := string(render(result))
	for _, want := range []string{
		"  npm/npmjs/-/unknown/1.0.0\n",
		"name the license without reproducing it:\n\n  a 1.0.0\n",
		"a 1.0.0\nLicense: ISC\n\n" + separator,
		"github.com/example/b v1.0.0\nLicense: MIT\n\nCopyright 1 B\nCopyright 2 B\n\nMIT text\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render() missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "a 1.0.0\nLicense") > strings.Index(got, "github.com/example/b") {
		t.Error("render() did not sort packages by name")
	}
}

func TestFetchNoticesFailsOnClientError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid coordinates", http.StatusBadRequest)
	}))
	defer server.Close()
	_, err := fetchNotices(server.Client(), server.URL, []string{"bad"}, 25)
	if err == nil || !strings.Contains(err.Error(), "invalid coordinates") {
		t.Fatalf("fetchNotices() error = %v, want client error", err)
	}
}

// The Publisher's Git source archive has no ClearlyDefined coordinate, so
// NOTICE.md must carry a hand-written section for the version it ships.
func TestNoticeCoversPublisherGitSource(t *testing.T) {
	t.Parallel()
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "..", "workers", "publisher", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`https://www\.kernel\.org/pub/software/scm/git/git-([0-9.]+)\.tar\.xz`).
		FindSubmatch(dockerfile)
	if match == nil {
		t.Fatal("publisher Dockerfile no longer downloads a Git source archive; update this test")
	}
	notice, err := os.ReadFile(filepath.Join("..", "..", "..", "NOTICE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Git " + string(match[1]); !strings.Contains(string(notice), want) {
		t.Errorf("NOTICE.md does not mention %q shipped by the Publisher image", want)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for header, want := range map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		"-3":                            0,
		"3600":                          5 * time.Minute,
		"Mon, 19 Oct 2026 12:00:30 GMT": 30 * time.Second,
		"Mon, 19 Oct 2026 11:59:00 GMT": 0,
		"soon":                          0,
	} {
		if got := retryAfterDelay(header, now); got != want {
			t.Errorf("retryAfterDelay(%q) = %v, want %v", header, got, want)
		}
	}
}
