/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package browser

import (
	"os"
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
