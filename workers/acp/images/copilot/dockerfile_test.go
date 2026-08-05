package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
)

func TestDockerfilePinsCopilotRuntime(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	required := []string{
		"docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e",
		"docker.io/library/golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19",
		"docker.io/library/node:22.22.0-bookworm-slim@sha256:" +
			"dd9d21971ec4395903fa6143c2b9267d048ae01ca6d3ea96f16cb30df6187d94",
		"https://github.com/github/copilot-cli/releases/download/v1.0.77/copilot-linux-x64.tar.gz",
		"https://github.com/github/copilot-cli/releases/download/v1.0.77/copilot-linux-arm64.tar.gz",
		"c6414f99c5b29b049a3b0929ba824f0ff0ae88b85eb52559be45631b96b00f4c",
		"5bcf01b30bd74ce415cc93acb404885e0bc396cde037ca68efe2b8ec84f91e5a",
		"https://raw.githubusercontent.com/github/copilot-cli/aee1edd29ef0f2058425bf399bcc9e5002a2b8f2/LICENSE.md",
		"1fbd0dcc55c66738b1b591632132c927de20c8443dff1d55b4851e378883e402",
		`CopilotCLIVersion[[:space:]]*= "1\.0\.77"`,
		`io.orka.acp.adapter.version="1.0.77"`,
		"test \"$(tar -tzf /artifacts/copilot.tgz)\" = \"copilot\"",
		"COPY --from=supervisor-builder /out/orka-acp-runtime /usr/local/bin/orka-acp-runtime",
		"COPY --from=supervisor-builder /out/orka-acp-exec-helper /usr/local/bin/orka-acp-exec-helper",
		"SSL_CERT_DIR=/etc/ssl/certs",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
		"COPY --from=upstream-artifacts /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt",
		"RUN test -s /etc/ssl/certs/ca-certificates.crt",
		"COPY --from=upstream-artifacts /artifacts/copilot-LICENSE.md /usr/share/licenses/github-copilot-cli/LICENSE.md",
		"test ! -e /usr/bin/git && test ! -e /usr/bin/ssh",
		"ENTRYPOINT [\"/usr/local/bin/orka-acp-runtime\"]",
	}
	for _, value := range required {
		if !strings.Contains(contents, value) {
			t.Errorf("Dockerfile is missing %q", value)
		}
	}
	forbidden := []string{
		"apt-get ", "apk add", "git clone", "npm install", "RUN npx ", "RUN curl ", "RUN wget ", "ADD http",
		"GITHUB_TOKEN", "GH_TOKEN", "SSH_AUTH_SOCK",
	}
	for _, value := range forbidden {
		if strings.Contains(contents, value) {
			t.Errorf("Dockerfile contains forbidden mutable or SCM surface %q", value)
		}
	}
	for line := range strings.SplitSeq(contents, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "FROM ") && !strings.Contains(trimmed, "@sha256:") {
			t.Errorf("un-pinned base image: %s", line)
		}
	}
}

func TestCopilotPinIsNewerThanCredentiallessBYOKACPFixBoundary(t *testing.T) {
	t.Parallel()
	got := parseVersionCore(t, acp.CopilotCLIVersion)
	fixBoundary := [3]int{1, 0, 76}
	if compareVersionCore(got, fixBoundary) <= 0 {
		t.Fatalf(
			"Copilot CLI version %q must be newer than credentialless BYOK ACP fix boundary 1.0.76-0",
			acp.CopilotCLIVersion,
		)
	}
}

func parseVersionCore(t *testing.T, version string) [3]int {
	t.Helper()
	var parsed [3]int
	core := strings.SplitN(version, "-", 2)[0]
	if count, err := fmt.Sscanf(core, "%d.%d.%d", &parsed[0], &parsed[1], &parsed[2]); err != nil || count != len(parsed) {
		t.Fatalf("parse Copilot CLI version %q: parsed %d components: %v", version, count, err)
	}
	return parsed
}

func compareVersionCore(left, right [3]int) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

func TestDockerContextIncludesCopilotLicenseInputs(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	for _, pattern := range []string{"!LICENSE", "!NOTICE.md"} {
		if !strings.Contains(contents, pattern) {
			t.Errorf(".dockerignore is missing %q", pattern)
		}
	}
}
