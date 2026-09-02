# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

# The tag documents the human-readable version; every base image and downloaded
# provider artifact is selected by an immutable digest.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.0-bookworm@sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466 AS upstream-artifacts
ARG TARGETARCH
COPY workers/acp/images/artifact-tool.go /tmp/artifact-tool.go
COPY LICENSE /artifacts/orka-LICENSE
COPY NOTICE.md /artifacts/orka-NOTICE.md
RUN go build -buildvcs=false -trimpath -ldflags='-s -w' -o /usr/local/bin/artifact-tool /tmp/artifact-tool.go
RUN set -eu; \
    mkdir -p /artifacts /opt/copilot/bin; \
    case "$TARGETARCH" in \
      amd64) \
        copilot_url="https://github.com/github/copilot-cli/releases/download/v1.0.77/copilot-linux-x64.tar.gz"; \
        copilot_sha="c6414f99c5b29b049a3b0929ba824f0ff0ae88b85eb52559be45631b96b00f4c" ;; \
      arm64) \
        copilot_url="https://github.com/github/copilot-cli/releases/download/v1.0.77/copilot-linux-arm64.tar.gz"; \
        copilot_sha="5bcf01b30bd74ce415cc93acb404885e0bc396cde037ca68efe2b8ec84f91e5a" ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    artifact-tool download \
      "$copilot_url" "$copilot_sha" /artifacts/copilot.tgz 134217728; \
    test "$(tar -tzf /artifacts/copilot.tgz)" = "copilot"; \
    tar -xzf /artifacts/copilot.tgz -C /opt/copilot/bin; \
    test -f /opt/copilot/bin/copilot; \
    chmod 0755 /opt/copilot/bin/copilot; \
    artifact-tool download \
      "https://raw.githubusercontent.com/github/copilot-cli/aee1edd29ef0f2058425bf399bcc9e5002a2b8f2/LICENSE.md" \
      "1fbd0dcc55c66738b1b591632132c927de20c8443dff1d55b4851e378883e402" \
      /artifacts/copilot-LICENSE.md 65536

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.0-bookworm@sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466 AS supervisor-builder
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

# This pinned Node base supplies the glibc/libstdc++ runtime required by the
# official Copilot binary. Node and its package managers are removed below.
FROM --platform=$TARGETPLATFORM docker.io/library/node:22.22.0-bookworm-slim@sha256:dd9d21971ec4395903fa6143c2b9267d048ae01ca6d3ea96f16cb30df6187d94
LABEL org.opencontainers.image.title="Orka ACP GitHub Copilot runtime" \
      org.opencontainers.image.description="Orka harness v2 ACP supervisor with GitHub Copilot CLI 1.0.77" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      org.opencontainers.image.licenses="MIT AND LicenseRef-GitHub-Copilot-CLI" \
      io.orka.acp.provider="copilot" \
      io.orka.acp.adapter.version="1.0.77" \
      io.orka.acp.adapter.revision="aee1edd29ef0f2058425bf399bcc9e5002a2b8f2" \
      io.orka.acp.adapter.digest.amd64="sha256:c6414f99c5b29b049a3b0929ba824f0ff0ae88b85eb52559be45631b96b00f4c" \
      io.orka.acp.adapter.digest.arm64="sha256:5bcf01b30bd74ce415cc93acb404885e0bc396cde037ca68efe2b8ec84f91e5a"
ENV HOME=/root \
    ORKA_ACP_PROVIDER=copilot \
    SSL_CERT_DIR=/etc/ssl/certs \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    PATH=/opt/copilot/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
RUN set -eu; \
    rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
      /opt/yarn-v1.22.22; \
    rm -f /usr/local/bin/node /usr/local/bin/npm /usr/local/bin/npx \
      /usr/local/bin/corepack /usr/local/bin/pnpm /usr/local/bin/pnpx \
      /usr/local/bin/yarn /usr/local/bin/yarnpkg /usr/local/bin/git \
      /usr/local/bin/ssh /usr/bin/git /usr/bin/ssh /usr/bin/scp /usr/bin/sftp \
      /usr/bin/curl /usr/bin/wget; \
    test ! -e /usr/bin/git && test ! -e /usr/bin/ssh; \
    mkdir -p /sessions /usr/share/licenses/orka /usr/share/licenses/github-copilot-cli; \
    chmod 0711 /sessions
COPY --from=supervisor-builder /out/orka-acp-runtime /usr/local/bin/orka-acp-runtime
COPY --from=supervisor-builder /out/orka-acp-exec-helper /usr/local/bin/orka-acp-exec-helper
COPY --from=upstream-artifacts /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=upstream-artifacts /opt/copilot /opt/copilot
COPY --from=upstream-artifacts /artifacts/copilot-LICENSE.md /usr/share/licenses/github-copilot-cli/LICENSE.md
COPY --from=upstream-artifacts /artifacts/orka-LICENSE /usr/share/licenses/orka/LICENSE
COPY --from=upstream-artifacts /artifacts/orka-NOTICE.md /usr/share/licenses/orka/NOTICE.md
RUN test -s /etc/ssl/certs/ca-certificates.crt
WORKDIR /
# The supervisor must start as root to assign each child a unique UID/GID and
# prove process cleanup. Kubernetes drops all capabilities and adds back only
# the explicit identity/process-management set.
# trivy:ignore:DS-0002
USER 0:0
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/orka-acp-runtime"]
