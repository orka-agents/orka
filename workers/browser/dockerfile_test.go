/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestBrowserWorkerDockerfileIsPinnedAndHardened(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)

	for line := range strings.SplitSeq(contents, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "FROM ") && !regexp.MustCompile(`@sha256:[0-9a-f]{64}( |$)`).MatchString(trimmed) {
			t.Errorf("un-pinned base image: %s", line)
		}
	}

	// One version, declared once, and every install goes through it.
	versions := regexp.MustCompile(`(?m)^ARG PLAYWRIGHT_VERSION=(\S+)$`).FindAllStringSubmatch(contents, -1)
	if len(versions) != 1 || versions[0][1] != PlaywrightVersion {
		t.Fatalf("PLAYWRIGHT_VERSION = %v, want exactly one ARG pinned to %s", versions, PlaywrightVersion)
	}
	if installs := strings.Count(contents, "playwright@"); installs != 1 ||
		!strings.Contains(contents, `"playwright@${PLAYWRIGHT_VERSION}"`) {
		t.Errorf("found %d playwright installs, want exactly one of the pinned version", installs)
	}

	for _, forbidden := range []string{
		":latest", "ADD http", "RUN curl ", "RUN wget ",
		"GITHUB_" + "TOKEN", "ARG TO" + "KEN", "ARG API_" + "KEY",
	} {
		if strings.Contains(contents, forbidden) {
			t.Errorf("Dockerfile contains forbidden mutable or secret-bearing surface %q", forbidden)
		}
	}

	finalStage := contents[strings.LastIndex(contents, "\nFROM "):]
	for _, required := range []string{
		"PLAYWRIGHT_BROWSERS_PATH=/ms-playwright",
		"playwright install --with-deps chromium",
		`chmod -R a+rX "${PLAYWRIGHT_BROWSERS_PATH}"`,
		"usermod -l worker -d /tmp",
		"COPY LICENSE NOTICE.md /usr/share/licenses/orka/",
		"COPY --from=builder --chown=0:0 /out/worker /worker",
		"HOME=/tmp",
		"XDG_CACHE_HOME=/tmp/.cache",
		"XDG_CONFIG_HOME=/tmp/.config",
		"USER 1000:1000",
		`ENTRYPOINT ["/usr/local/bin/browser-entrypoint"]`,
		`exec /worker "$@"`,
		"/usr/local/bin/wait-for-network",
		`trap "exit 143" TERM INT`,
		"/proc/net/if_inet6",
		"--no-sandbox --disable-dev-shm-usage",
		"--user-data-dir=/tmp/chrome-profile",
		"/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-linux*/chrome-headless-shell",
		// playwright install would hang on the read-only root filesystem.
		"install|install-deps|chromium|chromium-headless-shell|ffmpeg|-*) ;;",
		"mv -f /usr/local/bin/playwright.orka /usr/local/bin/playwright",
		"cli=/usr/local/lib/node_modules/playwright/cli.js",
		`exec node "$cli" "$@"`,
		// Diagnostics and a browser path other than the image's reach the real installer.
		`[ "${PLAYWRIGHT_BROWSERS_PATH:-}" = /ms-playwright ] || exec node "$cli" "$@"`,
		`--help|-h|--list|--dry-run) exec node "$cli" "$@" ;;`,
	} {
		if !strings.Contains(finalStage, required) {
			t.Errorf("final stage is missing %q", required)
		}
	}
	// Full Chromium calls Google sign-in, sync, and update services by itself,
	// which a proxy log of any raw chromium-headless run would show.
	if strings.Contains(finalStage, "/ms-playwright/chromium-*/chrome-linux") {
		t.Error("chromium-headless must run the headless shell, not full Chromium")
	}
	if strings.Contains(finalStage, "\nCMD ") {
		t.Error("the final stage must not set CMD: a Task's args are the command the worker runs")
	}
}

// chromeWrapper is the script the image puts in front of each Chromium binary.
func chromeWrapper(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	start := strings.Index(contents, `mv "$chrome" "$chrome.orka"; \`)
	end := strings.Index(contents, `> "$chrome"; \`)
	if start < 0 || end < start {
		t.Fatal("the Chromium wrapper is missing from the Dockerfile")
	}
	var script strings.Builder
	for _, m := range regexp.MustCompile(`(?m)^\s*'(.*)' \\$`).FindAllStringSubmatch(contents[start:end], -1) {
		script.WriteString(m[1] + "\n")
	}
	return script.String()
}

// With live view on, Chromium opens a loopback DevTools port for the worker
// to screencast, except for the one-shot commands that refuse one ("Headless
// commands are not compatible with remote debugging").
func TestChromiumWrapperOpensDevToolsOnlyWhereChromiumAllowsIt(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	chrome := filepath.Join(dir, "chrome-headless-shell")
	if err := os.WriteFile(chrome, []byte(chromeWrapper(t)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chrome+".orka", []byte("#!/bin/sh\necho \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(liveView string, args ...string) string {
		cmd := exec.Command(chrome, args...)
		cmd.Env = append(os.Environ(), "ORKA_BROWSER_LIVE_VIEW="+liveView)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}

	playwright := []string{
		"--headless", "--remote-debugging-pipe", "--user-data-dir=/tmp/playwright_chromiumdev_profile-x",
	}
	if got, want := run("1", playwright...), "--remote-debugging-port=0 "+strings.Join(playwright, " "); got != want {
		t.Errorf("with live view on, Playwright's launch ran %q, want %q", got, want)
	}
	if got, want := run("", playwright...), strings.Join(playwright, " "); got != want {
		t.Errorf("with live view off, Playwright's launch ran %q, want %q", got, want)
	}
	for _, command := range [][]string{
		{"--screenshot=/tmp/artifacts/page.png", "--window-size=1280,800", "https://example.com"},
		{"--screenshot", "https://example.com"},
		{"--dump-dom", "https://example.com"},
		{"--print-to-pdf=/tmp/artifacts/page.pdf", "https://example.com"},
		{"-dump-dom", "https://example.com"},
	} {
		args := append([]string{"--no-sandbox"}, command...)
		if got, want := run("1", args...), strings.Join(args, " "); got != want {
			t.Errorf("a one-shot command ran %q, want %q: Chromium refuses it with a DevTools port", got, want)
		}
	}
}
