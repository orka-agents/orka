# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

# The tag documents the human-readable version; the manifest-list digest makes
# every supported platform immutable.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS upstream-artifacts
ARG TARGETARCH
COPY workers/acp/images/artifact-tool.go /tmp/artifact-tool.go
RUN go build -buildvcs=false -trimpath -ldflags='-s -w' -o /usr/local/bin/artifact-tool /tmp/artifact-tool.go
RUN set -eu; \
    mkdir -p /artifacts; \
    artifact-tool download \
      "https://codeload.github.com/agentclientprotocol/claude-agent-acp/tar.gz/c19bddcf7914259d6c15103a2d1580c7371e1d16" \
      "e927898fd1b9d32a9a070c4217a89ba793c2baa1b03a4ffb509b13236333cf36" \
      /artifacts/claude-agent-acp-source.tgz 16777216; \
    artifact-tool download \
      "https://registry.npmjs.org/@agentclientprotocol/claude-agent-acp/-/claude-agent-acp-0.61.0.tgz" \
      "bb410d53e2d13591da5ebcaf09ae055d166ffefb27b7172988650cd62ef4ebd3" \
      /artifacts/claude-agent-acp-release.tgz 16777216; \
    artifact-tool download \
      "https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.3.217.tgz" \
      "20363761b29724950b749ecbc5186c46e29f2a0330554ca309a9e7ff8d6e5799" \
      /artifacts/claude-agent-sdk.tgz 16777216; \
    case "$TARGETARCH" in \
      amd64) \
        native_url="https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk-linux-x64/-/claude-agent-sdk-linux-x64-0.3.217.tgz"; \
        native_sha="8d2ccffbdf63aef1436a30b0fd32ef0eac663c3465318b181138471a68f02b92" ;; \
      arm64) \
        native_url="https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk-linux-arm64/-/claude-agent-sdk-linux-arm64-0.3.217.tgz"; \
        native_sha="51080502df2942d2092c4a5b7122eda518041fbe954027a1e128ede3cb337563" ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    artifact-tool download \
      "$native_url" "$native_sha" /artifacts/claude-native.tgz 125829120; \
    artifact-tool download \
      "https://raw.githubusercontent.com/orka-agents/orka/d03acb995b6014a6e855181c50b922b65ea8e7ff/LICENSE" \
      "7df20dcdf9197e9945c14858d41c60f11b52b93e5b69e2b63416b874d598d322" \
      /artifacts/orka-LICENSE 65536; \
    artifact-tool download \
      "https://raw.githubusercontent.com/orka-agents/orka/d03acb995b6014a6e855181c50b922b65ea8e7ff/NOTICE.md" \
      "f0e9fca2012cfb65dd9532e0fe693c405de5006d23e54b99b78154060f7dbf75" \
      /artifacts/orka-NOTICE.md 65536

FROM --platform=$BUILDPLATFORM docker.io/library/node:22.22.0-bookworm-slim@sha256:dd9d21971ec4395903fa6143c2b9267d048ae01ca6d3ea96f16cb30df6187d94 AS adapter-builder
ENV NPM_CONFIG_AUDIT=false \
    NPM_CONFIG_FUND=false \
    NPM_CONFIG_UPDATE_NOTIFIER=false
COPY --from=upstream-artifacts /usr/local/bin/artifact-tool /usr/local/bin/artifact-tool
COPY --from=upstream-artifacts /artifacts/claude-agent-acp-source.tgz /tmp/source.tgz
COPY --from=upstream-artifacts /artifacts/claude-agent-acp-release.tgz /tmp/release.tgz
RUN set -eu; \
    mkdir -p /src /published; \
    tar -xzf /tmp/source.tgz -C /src --strip-components=1; \
    tar -xzf /tmp/release.tgz -C /published
WORKDIR /src
RUN npm ci --ignore-scripts --no-audit --no-fund --registry=https://registry.npmjs.org/
RUN npm run build && rm -rf /src/dist/tests
RUN artifact-tool compare /src/dist /published/package/dist

FROM --platform=$TARGETPLATFORM docker.io/library/node:22.22.0-bookworm-slim@sha256:dd9d21971ec4395903fa6143c2b9267d048ae01ca6d3ea96f16cb30df6187d94 AS claude-layout
ARG TARGETARCH
ENV NPM_CONFIG_AUDIT=false \
    NPM_CONFIG_FUND=false \
    NPM_CONFIG_UPDATE_NOTIFIER=false
WORKDIR /opt/claude-agent-acp
COPY --from=adapter-builder /src/package.json /src/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund --registry=https://registry.npmjs.org/
COPY --from=adapter-builder /src/dist ./dist
COPY --from=adapter-builder /src/README.md /src/LICENSE ./
COPY --from=upstream-artifacts /artifacts/claude-agent-sdk.tgz /tmp/claude-agent-sdk.tgz
COPY --from=upstream-artifacts /artifacts/claude-native.tgz /tmp/claude-native.tgz
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) native_package=claude-agent-sdk-linux-x64 ;; \
      arm64) native_package=claude-agent-sdk-linux-arm64 ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    rm -rf node_modules/@anthropic-ai/claude-agent-sdk; \
    mkdir -p node_modules/@anthropic-ai/claude-agent-sdk; \
    tar -xzf /tmp/claude-agent-sdk.tgz \
      -C node_modules/@anthropic-ai/claude-agent-sdk --strip-components=1; \
    rm -rf "node_modules/@anthropic-ai/$native_package"; \
    mkdir -p "node_modules/@anthropic-ai/$native_package"; \
    tar -xzf /tmp/claude-native.tgz \
      -C "node_modules/@anthropic-ai/$native_package" --strip-components=1; \
    node -e 'const p=require(`./node_modules/@anthropic-ai/${process.argv[1]}/package.json`); if(p.version!=="0.3.217") throw new Error(`unexpected Claude native package ${p.name}@${p.version}`)' "$native_package"; \
    test -x "node_modules/@anthropic-ai/$native_package/claude"; \
    mkdir -p /opt/claude/bin; \
    cp "node_modules/@anthropic-ai/$native_package/LICENSE.md" /opt/claude/LICENSE.md; \
    ln -s "/opt/claude-agent-acp/node_modules/@anthropic-ai/$native_package/claude" \
      /opt/claude/bin/claude

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS supervisor-builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download
COPY . .
RUN set -eu; \
    grep -Eq '^[[:space:]]*ClaudeACPVersion[[:space:]]*= "0\.61\.0"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*ClaudeACPSourceCommit[[:space:]]*= "c19bddcf7914259d6c15103a2d1580c7371e1d16"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*ClaudeACPTarSHA256[[:space:]]*= "bb410d53e2d13591da5ebcaf09ae055d166ffefb27b7172988650cd62ef4ebd3"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*ClaudeAgentSDKVersion[[:space:]]*= "0\.3\.217"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*ClaudeCodeVersion[[:space:]]*= "2\.1\.217"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*ClaudeSDKLinuxX64SHA256[[:space:]]*= "8d2ccffbdf63aef1436a30b0fd32ef0eac663c3465318b181138471a68f02b92"$' internal/acp/pins.go; \
    grep -Eq '^[[:space:]]*ClaudeSDKLinuxARM64SHA256[[:space:]]*= "51080502df2942d2092c4a5b7122eda518041fbe954027a1e128ede3cb337563"$' internal/acp/pins.go; \
    test "$TARGETOS" = linux; \
    case "$TARGETARCH" in amd64|arm64) ;; *) exit 1 ;; esac; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /out/orka-acp-runtime ./cmd/orka-acp-runtime; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /out/orka-acp-exec-helper ./cmd/orka-acp-exec-helper

FROM --platform=$TARGETPLATFORM docker.io/library/node:22.22.0-bookworm-slim@sha256:dd9d21971ec4395903fa6143c2b9267d048ae01ca6d3ea96f16cb30df6187d94
LABEL org.opencontainers.image.title="Orka ACP Claude runtime" \
      org.opencontainers.image.description="Orka harness v2 ACP supervisor with Claude Agent ACP 0.61.0 and Claude Agent SDK 0.3.217 / Claude Code 2.1.217" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      org.opencontainers.image.licenses="MIT AND Apache-2.0 AND LicenseRef-Anthropic-Legal-Agreements" \
      io.orka.acp.provider="claude" \
      io.orka.acp.adapter.version="0.61.0" \
      io.orka.acp.adapter.revision="c19bddcf7914259d6c15103a2d1580c7371e1d16" \
      io.orka.acp.adapter.digest="sha256:bb410d53e2d13591da5ebcaf09ae055d166ffefb27b7172988650cd62ef4ebd3" \
      io.orka.acp.provider.version="0.3.217" \
      io.orka.acp.provider.cli.version="2.1.217" \
      io.orka.acp.provider.digest.amd64="sha256:8d2ccffbdf63aef1436a30b0fd32ef0eac663c3465318b181138471a68f02b92" \
      io.orka.acp.provider.digest.arm64="sha256:51080502df2942d2092c4a5b7122eda518041fbe954027a1e128ede3cb337563"
ENV HOME=/root \
    NODE_ENV=production \
    ORKA_ACP_PROVIDER=claude \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
RUN set -eu; \
    ln -s /usr/local/bin/node /usr/bin/node; \
    rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
      /opt/yarn-v1.22.22; \
    rm -f /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
      /usr/local/bin/pnpm /usr/local/bin/pnpx /usr/local/bin/yarn /usr/local/bin/yarnpkg; \
    mkdir -p /sessions /usr/share/licenses/orka /usr/share/licenses/claude-agent-acp \
      /usr/share/licenses/claude-agent-sdk /usr/share/licenses/claude-code; \
    chmod 0711 /sessions
COPY --from=supervisor-builder /out/orka-acp-runtime /usr/local/bin/orka-acp-runtime
COPY --from=supervisor-builder /out/orka-acp-exec-helper /usr/local/bin/orka-acp-exec-helper
COPY --from=claude-layout /opt/claude-agent-acp /opt/claude-agent-acp
COPY --from=claude-layout /opt/claude /opt/claude
COPY --from=claude-layout /opt/claude-agent-acp/LICENSE /usr/share/licenses/claude-agent-acp/LICENSE
COPY --from=claude-layout /opt/claude-agent-acp/node_modules/@anthropic-ai/claude-agent-sdk/LICENSE.md /usr/share/licenses/claude-agent-sdk/LICENSE.md
COPY --from=claude-layout /opt/claude/LICENSE.md /usr/share/licenses/claude-code/LICENSE.md
COPY --from=upstream-artifacts /artifacts/orka-LICENSE /usr/share/licenses/orka/LICENSE
COPY --from=upstream-artifacts /artifacts/orka-NOTICE.md /usr/share/licenses/orka/NOTICE.md
WORKDIR /
# The supervisor must start as root to assign each child a unique UID/GID and
# prove process cleanup. Kubernetes drops all capabilities and adds back only
# the explicit identity/process-management set.
# trivy:ignore:DS-0002
USER 0:0
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/orka-acp-runtime"]
