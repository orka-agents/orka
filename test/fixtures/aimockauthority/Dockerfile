# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# Copyright (c) 2026. MIT License - see LICENSE file for details.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
COPY internal/transactiontoken/profile.go ./internal/transactiontoken/profile.go
COPY test/fixtures/aimockauthority ./test/fixtures/aimockauthority
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o /authority ./test/fixtures/aimockauthority/cmd
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
COPY --from=builder /authority /authority
USER 65532:65532
ENTRYPOINT ["/authority"]
