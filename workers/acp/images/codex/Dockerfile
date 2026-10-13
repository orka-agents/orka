# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

# The tag documents the human-readable version; the manifest-list digest makes
# every supported platform immutable.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS upstream-artifacts
ARG TARGETARCH
COPY workers/acp/images/artifact-tool.go /tmp/artifact-tool.go
COPY NOTICE.md /artifacts/orka-NOTICE.md
RUN go build -buildvcs=false -trimpath -ldflags='-s -w' -o /usr/local/bin/artifact-tool /tmp/artifact-tool.go
RUN set -eu; \
    mkdir -p /artifacts; \
    artifact-tool download \
      "https://codeload.github.com/agentclientprotocol/codex-acp/tar.gz/68d7d2d5ddfc0ed5746f9f6130892dda685e65dd" \
      "51ba8d1d302f8a854c0501887ffbe12fb267f6e8fdae9ad2b58e2c8fc9513b1e" \
      /artifacts/codex-acp-source.tgz 16777216; \
    artifact-tool download \
      "https://registry.npmjs.org/@agentclientprotocol/codex-acp/-/codex-acp-2.1.1.tgz" \
      "9da0d580518d006d257609a4b64b7c5bd96a7f36f2d86db5c8f2cb9385de5872" \
      /artifacts/codex-acp-release.tgz 16777216; \
    case "$TARGETARCH" in \
      amd64) \
        native_url="https://registry.npmjs.org/@openai/codex/-/codex-0.160.0-linux-x64.tgz"; \
        native_sha="37a41d61c3399182b8c727b77090cc7a1566bd849d0f09070a0bbc6fec4c58dc" ;; \
      arm64) \
        native_url="https://registry.npmjs.org/@openai/codex/-/codex-0.160.0-linux-arm64.tgz"; \
        native_sha="9286a7e01d500ab224c9e5b4b223b7adf5dd901fba23efd6a438316426798b17" ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    artifact-tool download \
      "$native_url" "$native_sha" /artifacts/codex-native.tgz 201326592; \
    artifact-tool download \
      "https://raw.githubusercontent.com/openai/codex/a956835d020762cb2b570053af06f643a11c0ecc/LICENSE" \
      "d17f227e4df5da1600391338865ce0f3055211760a36688f816941d58232d8dc" \
      /artifacts/codex-LICENSE 65536; \
    artifact-tool download \
      "https://raw.githubusercontent.com/openai/codex/a956835d020762cb2b570053af06f643a11c0ecc/NOTICE" \
      "9d71575ecfd9a843fc1677b0efb08053c6ba9fd686a0de1a6f5382fd3c220915" \
      /artifacts/codex-NOTICE 65536; \
    artifact-tool download \
      "https://raw.githubusercontent.com/orka-agents/orka/d03acb995b6014a6e855181c50b922b65ea8e7ff/LICENSE" \
      "7df20dcdf9197e9945c14858d41c60f11b52b93e5b69e2b63416b874d598d322" \
      /artifacts/orka-LICENSE 65536

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
    printf '%s  %s\n' '538ac5d7ae6e3d46f3f0961439f914bfb9e427be6aea83c9b1025548ef8cac1d' /tmp/patch-agent-mode.mjs | sha256sum -c -; \
    node /tmp/patch-agent-mode.mjs /src/src/AgentMode.ts; \
    npm run build; \
    printf '%s  %s\n' '6792f51a6af4e1ae6317ad2197240836038bc53cb9f0fc9152a62d31b132d5a2' /src/dist/index.js | sha256sum -c -

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
    node -e 'const p=require("/tmp/native/package/package.json"); if(p.name!=="@openai/codex"||p.version!==`0.160.0-linux-${process.argv[1]}`) throw new Error(`unexpected Codex package ${p.name}@${p.version}`)' "$package_arch"; \
    test -x "/tmp/native/package/vendor/$target_triple/bin/codex"; \
    mv /tmp/native/package/vendor /opt/codex/vendor; \
    cp /tmp/native/package/README.md /opt/codex/README.md; \
    cp /tmp/native/package/package.json /opt/codex/package.json; \
    ln -s "../vendor/$target_triple/bin/codex" /opt/codex/bin/codex

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS supervisor-builder
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
      org.opencontainers.image.description="Orka harness v2 ACP supervisor with Codex ACP 2.1.1 and Codex CLI 0.160.0" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      org.opencontainers.image.licenses="MIT AND Apache-2.0" \
      ai.orka.acp.provider="codex" \
      ai.orka.acp.adapter.version="2.1.1" \
      ai.orka.acp.adapter.revision="68d7d2d5ddfc0ed5746f9f6130892dda685e65dd" \
      ai.orka.acp.adapter.digest="sha256:9da0d580518d006d257609a4b64b7c5bd96a7f36f2d86db5c8f2cb9385de5872" \
      ai.orka.acp.adapter.patch.digest="sha256:538ac5d7ae6e3d46f3f0961439f914bfb9e427be6aea83c9b1025548ef8cac1d" \
      ai.orka.acp.adapter.dist.digest="sha256:6792f51a6af4e1ae6317ad2197240836038bc53cb9f0fc9152a62d31b132d5a2" \
      ai.orka.acp.provider.version="0.160.0" \
      ai.orka.acp.provider.revision="a956835d020762cb2b570053af06f643a11c0ecc" \
      ai.orka.acp.provider.digest.amd64="sha256:37a41d61c3399182b8c727b77090cc7a1566bd849d0f09070a0bbc6fec4c58dc" \
      ai.orka.acp.provider.digest.arm64="sha256:9286a7e01d500ab224c9e5b4b223b7adf5dd901fba23efd6a438316426798b17"
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
