package publisher

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfilePinsCleanRoomProfile(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	required := []string{
		"docker.io/library/debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818",
		"docker.io/library/golang:1.27.0-bookworm@sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466",
		"GIT_VERSION=2.55.0",
		"FROM --platform=$TARGETPLATFORM docker.io/library/debian:bookworm-slim@sha256:" +
			"7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS git-builder",
		"457fdb04dc8728e007d4688695e6912e6f680727920f2a40bf11eacc17505357",
		"e9019fcafe0040228b8631c30f97ae1adb61bcdc",
		"ADD --checksum=sha256:",
		"NO_RUST=YesPlease",
		"USER 65532:65532",
		"io.orka.network.identity=\"workspace-publisher\"",
		"io.orka.provider-access=\"false\"",
		"io.orka.mcp-access=\"false\"",
		"COPY --from=git-builder /src/COPYING /usr/share/licenses/git/COPYING",
		"ENTRYPOINT [\"/usr/local/bin/orka-workspace-publisher\"]",
	}
	for _, value := range required {
		if !strings.Contains(contents, value) {
			t.Errorf("Dockerfile is missing %q", value)
		}
	}
	forbidden := []string{
		"apt-get install -y git", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CODEX_API_KEY",
		"MCP_SERVER", "npx ", "npm install", "curl ", "wget ",
		"FROM --platform=$BUILDPLATFORM docker.io/library/debian:bookworm-slim@sha256:" +
			"7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS git-builder",
	}
	for _, value := range forbidden {
		if strings.Contains(contents, value) {
			t.Errorf("Dockerfile contains forbidden mutable/provider surface %q", value)
		}
	}
	for line := range strings.SplitSeq(contents, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "FROM ") && !strings.Contains(trimmed, "@sha256:") {
			t.Errorf("un-pinned base image: %s", line)
		}
	}
}
