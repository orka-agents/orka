package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
)

func TestDockerfilePinsOpenCodeArtifactsAndRuntime(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	for _, value := range []string{
		"docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e",
		"docker.io/library/golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19",
		"docker.io/library/debian:trixie-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd",
		"https://codeload.github.com/anomalyco/opencode/tar.gz/" + acp.OpenCodeSourceCommit,
		"https://registry.npmjs.org/opencode-linux-x64-baseline/-/" +
			"opencode-linux-x64-baseline-" + acp.OpenCodeVersion + ".tgz",
		"https://registry.npmjs.org/opencode-linux-arm64/-/opencode-linux-arm64-" + acp.OpenCodeVersion + ".tgz",
		acp.OpenCodeSourceTarSHA256,
		acp.OpenCodeLinuxX64BaselineTarSHA256,
		acp.OpenCodeLinuxARM64TarSHA256,
		acp.OpenCodeLinuxX64BinarySHA256,
		acp.OpenCodeLinuxARM64BinarySHA256,
		acp.OpenCodeRipgrepVersion,
		acp.OpenCodeRipgrepDebianVersion,
		acp.OpenCodeRipgrepLinuxX64DebSHA256,
		acp.OpenCodeRipgrepLinuxARM64DebSHA256,
		acp.OpenCodeRipgrepLinuxX64BinarySHA256,
		acp.OpenCodeRipgrepLinuxARM64BinarySHA256,
		"https://snapshot.debian.org/file/096560a159a8be70155f16209d91777019011677",
		"https://snapshot.debian.org/file/482bbe93dc82997d7d84c901990e8d9f4327457c",
		"https://deb.debian.org/debian/pool/main/r/rust-ripgrep/ripgrep_15.2.0-1_amd64.deb",
		"https://deb.debian.org/debian/pool/main/r/rust-ripgrep/ripgrep_15.2.0-1_arm64.deb",
		acp.OpenCodeRootInstructionSHA256,
		acp.OpenCodeImageNoticeSHA256,
		"ORKA_ACP_PROVIDER=opencode",
		"COPY --from=opencode-layout /opt/opencode/bin/opencode /opt/opencode/bin/opencode",
		"COPY --from=opencode-layout /opt/ripgrep/bin/rg /usr/local/bin/rg",
		"COPY --from=opencode-layout /opt/ripgrep/licenses/COPYRIGHT /usr/share/licenses/ripgrep/COPYRIGHT",
		"COPY --from=upstream-artifacts /artifacts/opencode-AGENTS.md /opt/opencode/AGENTS.md",
		"ENTRYPOINT [\"/usr/local/bin/orka-acp-runtime\"]",
	} {
		if !strings.Contains(content, value) {
			t.Errorf("Dockerfile is missing %q", value)
		}
	}

	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "FROM ") && !strings.Contains(line, "@sha256:") {
			t.Errorf("Dockerfile contains mutable base image: %s", line)
		}
	}
	for _, forbidden := range []string{
		"OPENAI_API_" + "KEY=",
		"OPENCODE_SERVER_" + "PASS" + "WORD=",
		"ARG API_" + "KEY",
		"ARG TO" + "KEN",
		"curl ",
		"wget ",
		"git clone",
		"unknown-linux-musl",
		"npm install",
		"npx ",
		":latest",
	} {
		if strings.Contains(content, forbidden) {
			t.Errorf("Dockerfile contains forbidden mutable or secret-bearing surface %q", forbidden)
		}
	}
}

func TestRootInstructionIsPinnedAndFailClosed(t *testing.T) {
	data, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != acp.OpenCodeRootInstructionSHA256 {
		t.Fatalf("AGENTS.md SHA-256 = %s, want %s", got, acp.OpenCodeRootInstructionSHA256)
	}
	content := string(data)
	for _, required := range []string{
		"workspace, provider proxy, MCP broker, and permission policy are authoritative",
		"Do not inspect or modify other session trees",
		"Never bypass an OpenCode allow/deny decision",
		"Do not change OpenCode configuration",
		"Never print, copy, persist, or expose credentials",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("AGENTS.md is missing guardrail %q", required)
		}
	}
}

func TestImageNoticeIsPinnedAndIncludesOpenCodeLicense(t *testing.T) {
	data, err := os.ReadFile("NOTICE.md")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != acp.OpenCodeImageNoticeSHA256 {
		t.Fatalf("NOTICE.md SHA-256 = %s, want %s", got, acp.OpenCodeImageNoticeSHA256)
	}
	root, err := os.ReadFile("../../../../NOTICE.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(root) != string(data) {
		t.Fatal("image NOTICE.md does not match the repository third-party notice")
	}
	content := string(data)
	for _, required := range []string{
		"## OpenCode",
		"OpenCode 1.18.9 native binary",
		"Copyright (c) 2025 opencode",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("NOTICE.md is missing %q", required)
		}
	}
}
