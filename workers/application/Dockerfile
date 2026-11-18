# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

FROM --platform=$TARGETPLATFORM docker.io/library/golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /build
ENV TMPDIR=/build/work GOTOOLCHAIN=local
RUN mkdir -p /build/work /artifacts
COPY go.mod go.sum ./
COPY internal/patchverification/ ./internal/patchverification/
COPY pkg/storage/v1/verification/ ./pkg/storage/v1/verification/
COPY workers/application/ ./workers/application/
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    set -eu; \
    test "$TARGETOS" = linux; \
    case "$TARGETARCH" in amd64|arm64) ;; *) exit 1 ;; esac; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -buildvcs=false -trimpath -ldflags='-s -w' \
      -o /artifacts/orka-application-worker ./workers/application; \
    chmod 0555 /artifacts/orka-application-worker

FROM docker.io/library/golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c
LABEL org.opencontainers.image.title="Orka application verification worker" \
      org.opencontainers.image.description="Fixed offline Go builds, measured application execution and independent API/TCP observation" \
      org.opencontainers.image.source="https://github.com/orka-agents/orka" \
      org.opencontainers.image.licenses="MIT"
COPY --from=builder /artifacts/orka-application-worker /usr/local/bin/orka-application-worker
WORKDIR /work
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/orka-application-worker"]
