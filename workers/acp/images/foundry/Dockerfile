# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG FOUNDRY_RUNTIME_IMAGE
ARG FOUNDRY_ADAPTER_DIGEST

FROM --platform=$TARGETPLATFORM ${FOUNDRY_RUNTIME_IMAGE} AS foundry

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.0-bookworm@sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466 AS supervisor-builder
ARG TARGETOS
ARG TARGETARCH
ARG FOUNDRY_RUNTIME_IMAGE
ARG FOUNDRY_ADAPTER_DIGEST
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download
COPY . .
COPY --from=foundry /agent-runtime-foundry /out/agent-runtime-foundry
COPY --from=foundry /agent/foundry.json /out/agent/foundry.json
RUN set -eu; \
    case "$FOUNDRY_RUNTIME_IMAGE" in *@sha256:*) ;; *) echo "FOUNDRY_RUNTIME_IMAGE must be digest-pinned" >&2; exit 1 ;; esac; \
    digest="${FOUNDRY_RUNTIME_IMAGE##*@sha256:}"; \
    test "${#digest}" -eq 64; \
    case "$digest" in *[!0-9a-f]*) echo "FOUNDRY_RUNTIME_IMAGE must use a lowercase sha256 digest" >&2; exit 1 ;; esac; \
    test "$FOUNDRY_ADAPTER_DIGEST" = "sha256:$digest" || { echo "FOUNDRY_ADAPTER_DIGEST must equal the Foundry source image digest" >&2; exit 1; }; \
    test -x /out/agent-runtime-foundry; \
    test -s /out/agent/foundry.json; \
    test "$TARGETOS" = linux; \
    case "$TARGETARCH" in amd64|arm64) ;; *) exit 1 ;; esac; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /out/orka-acp-runtime ./cmd/orka-acp-runtime; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /out/orka-acp-exec-helper ./cmd/orka-acp-exec-helper; \
    mkdir -p /out/sessions; \
    chmod 0711 /out/sessions; \
    chmod 0555 /out/agent /out/agent-runtime-foundry; \
    chmod 0444 /out/agent/foundry.json

# The configured source image supplies a static ACP child and its immutable
# target/model/tool policy. Azure identity belongs only to the broker sidecar.
FROM --platform=$TARGETPLATFORM ${FOUNDRY_RUNTIME_IMAGE}
ARG FOUNDRY_ADAPTER_DIGEST
LABEL org.opencontainers.image.title="Orka ACP Foundry runtime" \
      org.opencontainers.image.description="Orka harness v2 supervisor with a Foundry ACP child" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      io.orka.acp.provider="foundry" \
      io.orka.acp.adapter.name="foundry-serve-acp"
ENV ORKA_ACP_PROVIDER=foundry \
    ORKA_ACP_FOUNDRY_ADAPTER_DIGEST=${FOUNDRY_ADAPTER_DIGEST}
COPY --from=supervisor-builder --chown=0:0 /out/orka-acp-runtime /usr/local/bin/orka-acp-runtime
COPY --from=supervisor-builder --chown=0:0 /out/orka-acp-exec-helper /usr/local/bin/orka-acp-exec-helper
COPY --from=supervisor-builder --chown=0:0 /out/agent-runtime-foundry /agent-runtime-foundry
COPY --from=supervisor-builder --chown=0:0 /out/agent /agent
COPY --from=supervisor-builder --chown=0:0 /out/sessions /sessions
COPY LICENSE /usr/share/licenses/orka/LICENSE
COPY NOTICE.md /usr/share/licenses/orka/NOTICE.md
WORKDIR /
# The supervisor allocates a distinct UID/GID for each child. Use the existing
# ACP Pod capability restrictions; the child receives no Azure credentials.
# trivy:ignore:DS-0002
USER 0:0
EXPOSE 8080
STOPSIGNAL SIGTERM
CMD []
ENTRYPOINT ["/usr/local/bin/orka-acp-runtime"]
