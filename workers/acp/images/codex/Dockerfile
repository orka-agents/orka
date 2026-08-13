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
      "https://codeload.github.com/agentclientprotocol/codex-acp/tar.gz/307d81018f7cc0c3141ddf71c7532d38310e2cfb" \
      "4d3cc46a901bdd4abf112e703e734078e09a172ecd535aa3e556318a051a7c39" \
      /artifacts/codex-acp-source.tgz 16777216; \
    artifact-tool download \
      "https://registry.npmjs.org/@agentclientprotocol/codex-acp/-/codex-acp-1.1.7.tgz" \
      "642920240baa0b6b1951fb2c56b9ff11689648019499615f941000d95d127301" \
      /artifacts/codex-acp-release.tgz 16777216; \
    case "$TARGETARCH" in \
      amd64) \
        native_url="https://registry.npmjs.org/@openai/codex/-/codex-0.145.0-linux-x64.tgz"; \
        native_sha="11239480f8e3efd1430f23bbe91c1a397856b8bbe6185ccbaee2382d25e03df2" ;; \
      arm64) \
        native_url="https://registry.npmjs.org/@openai/codex/-/codex-0.145.0-linux-arm64.tgz"; \
        native_sha="b78c57e172b2f18e5969ae26183253cd3cdd9abb3b424a8f7334f4b5530c2b27" ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    artifact-tool download \
      "$native_url" "$native_sha" /artifacts/codex-native.tgz 201326592; \
    artifact-tool download \
      "https://raw.githubusercontent.com/openai/codex/25af12f7e61572b0bc18ddb1008be543b91519b0/LICENSE" \
      "d17f227e4df5da1600391338865ce0f3055211760a36688f816941d58232d8dc" \
      /artifacts/codex-LICENSE 65536; \
    artifact-tool download \
      "https://raw.githubusercontent.com/openai/codex/25af12f7e61572b0bc18ddb1008be543b91519b0/NOTICE" \
      "9d71575ecfd9a843fc1677b0efb08053c6ba9fd686a0de1a6f5382fd3c220915" \
      /artifacts/codex-NOTICE 65536; \
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
COPY --from=upstream-artifacts /artifacts/codex-acp-source.tgz /tmp/source.tgz
COPY --from=upstream-artifacts /artifacts/codex-acp-release.tgz /tmp/release.tgz
COPY workers/acp/images/codex/patch-agent-mode.mjs /tmp/patch-agent-mode.mjs
RUN set -eu; \
    mkdir -p /src /published; \
    tar -xzf /tmp/source.tgz -C /src --strip-components=1; \
    tar -xzf /tmp/release.tgz -C /published
WORKDIR /src
RUN npm ci --ignore-scripts --no-audit --no-fund --registry=https://registry.npmjs.org/
RUN npm run build
RUN artifact-tool compare /src/dist /published/package/dist
RUN set -eu; \
    printf '%s  %s\n' '4b1fc39dc7ac0d6aa404030a3433f46c8b9c67e8e8223efca490a6ab8bf4287a' /tmp/patch-agent-mode.mjs | sha256sum -c -; \
    node /tmp/patch-agent-mode.mjs /src/src/AgentMode.ts; \
    npm run build; \
    printf '%s  %s\n' 'bcfb6a2772a7de2d027e977d9bc9db5fa210a462de1af7ab061361be4410cc75' /src/dist/index.js | sha256sum -c -

FROM --platform=$BUILDPLATFORM docker.io/library/node:22.22.0-bookworm-slim@sha256:dd9d21971ec4395903fa6143c2b9267d048ae01ca6d3ea96f16cb30df6187d94 AS codex-layout
ARG TARGETARCH
COPY --from=upstream-artifacts /artifacts/codex-native.tgz /tmp/codex-native.tgz
RUN set -eu; \
    mkdir -p /tmp/native /opt/codex/bin; \
    tar -xzf /tmp/codex-native.tgz -C /tmp/native; \
    case "$TARGETARCH" in \
      amd64) package_arch=x64; target_triple=x86_64-unknown-linux-musl ;; \
      arm64) package_arch=arm64; target_triple=aarch64-unknown-linux-musl ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    node -e 'const p=require("/tmp/native/package/package.json"); if(p.name!=="@openai/codex"||p.version!==`0.145.0-linux-${process.argv[1]}`) throw new Error(`unexpected Codex package ${p.name}@${p.version}`)' "$package_arch"; \
    test -x "/tmp/native/package/vendor/$target_triple/bin/codex"; \
    mv /tmp/native/package/vendor /opt/codex/vendor; \
    cp /tmp/native/package/README.md /opt/codex/README.md; \
    cp /tmp/native/package/package.json /opt/codex/package.json; \
    ln -s "../vendor/$target_triple/bin/codex" /opt/codex/bin/codex

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS supervisor-builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download
COPY . .
RUN set -eu; \
    test "$TARGETOS" = linux; \
    case "$TARGETARCH" in amd64|arm64) ;; *) exit 1 ;; esac; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /out/orka-acp-runtime ./cmd/orka-acp-runtime; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /out/orka-acp-exec-helper ./cmd/orka-acp-exec-helper

FROM --platform=$TARGETPLATFORM docker.io/library/node:22.22.0-bookworm-slim@sha256:dd9d21971ec4395903fa6143c2b9267d048ae01ca6d3ea96f16cb30df6187d94
LABEL org.opencontainers.image.title="Orka ACP Codex runtime" \
      org.opencontainers.image.description="Orka harness v2 ACP supervisor with Codex ACP 1.1.7 and Codex CLI 0.145.0" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      org.opencontainers.image.licenses="MIT AND Apache-2.0" \
      io.orka.acp.provider="codex" \
      io.orka.acp.adapter.version="1.1.7" \
      io.orka.acp.adapter.revision="307d81018f7cc0c3141ddf71c7532d38310e2cfb" \
      io.orka.acp.adapter.digest="sha256:642920240baa0b6b1951fb2c56b9ff11689648019499615f941000d95d127301" \
      io.orka.acp.adapter.patch.digest="sha256:4b1fc39dc7ac0d6aa404030a3433f46c8b9c67e8e8223efca490a6ab8bf4287a" \
      io.orka.acp.adapter.dist.digest="sha256:bcfb6a2772a7de2d027e977d9bc9db5fa210a462de1af7ab061361be4410cc75" \
      io.orka.acp.provider.version="0.145.0" \
      io.orka.acp.provider.revision="25af12f7e61572b0bc18ddb1008be543b91519b0" \
      io.orka.acp.provider.digest.amd64="sha256:11239480f8e3efd1430f23bbe91c1a397856b8bbe6185ccbaee2382d25e03df2" \
      io.orka.acp.provider.digest.arm64="sha256:b78c57e172b2f18e5969ae26183253cd3cdd9abb3b424a8f7334f4b5530c2b27"
ENV HOME=/root \
    NODE_ENV=production \
    ORKA_ACP_PROVIDER=codex \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
RUN set -eu; \
    ln -s /usr/local/bin/node /usr/bin/node; \
    rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
      /opt/yarn-v1.22.22; \
    rm -f /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
      /usr/local/bin/pnpm /usr/local/bin/pnpx /usr/local/bin/yarn /usr/local/bin/yarnpkg; \
    mkdir -p /sessions /opt/codex-acp/dist /usr/share/licenses/orka \
      /usr/share/licenses/codex-acp /usr/share/licenses/codex; \
    chmod 0711 /sessions
COPY --from=supervisor-builder /out/orka-acp-runtime /usr/local/bin/orka-acp-runtime
COPY --from=supervisor-builder /out/orka-acp-exec-helper /usr/local/bin/orka-acp-exec-helper
COPY --from=adapter-builder /src/dist/index.js /opt/codex-acp/dist/index.js
COPY --from=adapter-builder /src/package.json /src/README.md /opt/codex-acp/
COPY --from=adapter-builder /src/LICENSE /usr/share/licenses/codex-acp/LICENSE
COPY --from=codex-layout /opt/codex /opt/codex
COPY --from=upstream-artifacts /artifacts/codex-LICENSE /usr/share/licenses/codex/LICENSE
COPY --from=upstream-artifacts /artifacts/codex-NOTICE /usr/share/licenses/codex/NOTICE
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
