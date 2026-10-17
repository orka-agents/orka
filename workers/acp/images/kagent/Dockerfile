# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG KAGENT_RUNTIME_IMAGE
ARG KAGENT_ADAPTER_DIGEST

FROM --platform=$TARGETPLATFORM ${KAGENT_RUNTIME_IMAGE} AS kagent

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS supervisor-builder
ARG TARGETOS
ARG TARGETARCH
ARG KAGENT_RUNTIME_IMAGE
ARG KAGENT_ADAPTER_DIGEST
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download
COPY . .
COPY --from=kagent /orka-runtime-kagent /out/orka-runtime-kagent
COPY --from=kagent /agent/kagent.json /out/agent/kagent.json
RUN set -eu; \
    case "$KAGENT_RUNTIME_IMAGE" in *@sha256:*) ;; *) echo "KAGENT_RUNTIME_IMAGE must be digest-pinned" >&2; exit 1 ;; esac; \
    digest="${KAGENT_RUNTIME_IMAGE##*@sha256:}"; \
    test "${#digest}" -eq 64; \
    case "$digest" in *[!0-9a-f]*) echo "KAGENT_RUNTIME_IMAGE must use a lowercase sha256 digest" >&2; exit 1 ;; esac; \
    test "$KAGENT_ADAPTER_DIGEST" = "sha256:$digest" || { echo "KAGENT_ADAPTER_DIGEST must equal the kagent source image digest" >&2; exit 1; }; \
    test -x /out/orka-runtime-kagent; \
    test -s /out/agent/kagent.json; \
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
    chmod 0555 /out/agent /out/orka-runtime-kagent; \
    chmod 0444 /out/agent/kagent.json

# The configured source image supplies a static ACP child and its immutable
# kagent target and model. kagent identity belongs only to the broker sidecar.
FROM --platform=$TARGETPLATFORM ${KAGENT_RUNTIME_IMAGE}
ARG KAGENT_ADAPTER_DIGEST
LABEL org.opencontainers.image.title="Orka ACP kagent runtime" \
      org.opencontainers.image.description="Orka harness v2 supervisor with a kagent ACP child" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      ai.orka.acp.provider="kagent" \
      ai.orka.acp.adapter.name="kagent-serve-acp"
ENV ORKA_ACP_PROVIDER=kagent \
    ORKA_ACP_KAGENT_ADAPTER_DIGEST=${KAGENT_ADAPTER_DIGEST}
COPY --from=supervisor-builder --chown=0:0 /out/orka-acp-runtime /usr/local/bin/orka-acp-runtime
COPY --from=supervisor-builder --chown=0:0 /out/orka-acp-exec-helper /usr/local/bin/orka-acp-exec-helper
COPY --from=supervisor-builder --chown=0:0 /out/orka-runtime-kagent /orka-runtime-kagent
COPY --from=supervisor-builder --chown=0:0 /out/agent /agent
COPY --from=supervisor-builder --chown=0:0 /out/sessions /sessions
COPY LICENSE /usr/share/licenses/orka/LICENSE
COPY NOTICE.md /usr/share/licenses/orka/NOTICE.md
WORKDIR /
# The supervisor allocates a distinct UID/GID for each child. Use the existing
# ACP Pod capability restrictions; the child receives no kagent credentials.
# trivy:ignore:DS-0002
USER 0:0
EXPOSE 8080
STOPSIGNAL SIGTERM
CMD []
ENTRYPOINT ["/usr/local/bin/orka-acp-runtime"]
