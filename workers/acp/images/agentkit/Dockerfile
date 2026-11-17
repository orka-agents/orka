# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG AGENTKIT_RUNTIME_IMAGE
ARG AGENTKIT_ADAPTER_DIGEST

FROM --platform=$TARGETPLATFORM ${AGENTKIT_RUNTIME_IMAGE} AS runtime-source

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

FROM supervisor-builder AS runtime-preparer
ARG AGENTKIT_RUNTIME_IMAGE
ARG AGENTKIT_ADAPTER_DIGEST
RUN --mount=from=runtime-source,target=/runtime,ro \
    sh workers/acp/images/agentkit/prepare-overlay.sh /runtime /out/prepared

# The source image is one AgentKit-built agent. It supplies the Python runtime,
# /opt/agentkit/bin/agentkit-serve, and the exact /agent/agent.yaml registered
# with Orka. The Orka layer adds only its static supervisor processes.
FROM --platform=$TARGETPLATFORM ${AGENTKIT_RUNTIME_IMAGE}
ARG AGENTKIT_RUNTIME_IMAGE
ARG AGENTKIT_ADAPTER_DIGEST
LABEL org.opencontainers.image.title="Orka ACP AgentKit runtime" \
      org.opencontainers.image.description="Orka harness v2 ACP supervisor layered onto a frozen AgentKit agent" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      ai.orka.acp.provider="agentkit" \
      ai.orka.acp.adapter.name="agentkit-serve-acp"
ENV ORKA_ACP_PROVIDER=agentkit \
    ORKA_ACP_AGENTKIT_ADAPTER_DIGEST=${AGENTKIT_ADAPTER_DIGEST}
USER 0:0
COPY --from=runtime-preparer /out/prepared/ /
COPY --from=supervisor-builder /out/orka-acp-runtime /usr/local/bin/orka-acp-runtime
COPY --from=supervisor-builder /out/orka-acp-exec-helper /usr/local/bin/orka-acp-exec-helper
COPY LICENSE /usr/share/licenses/orka/LICENSE
COPY NOTICE.md /usr/share/licenses/orka/NOTICE.md
WORKDIR /
# The supervisor starts as root so it can allocate a unique UID/GID for each
# child and prove descendant cleanup. The Pod security context drops every
# capability except the existing identity and process-management set.
# trivy:ignore:DS-0002
USER 0:0
EXPOSE 8080
STOPSIGNAL SIGTERM
CMD []
ENTRYPOINT ["/usr/local/bin/orka-acp-runtime"]
