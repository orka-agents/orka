# Build stage
FROM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy source
COPY . .

# Build the worker
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -o worker ./workers/general

# Runtime stage
FROM alpine:3.24

RUN apk add --no-cache ca-certificates git \
    && addgroup -g 1000 worker \
    && adduser -D -u 1000 -G worker worker \
    && mkdir -p /workspace /home/worker /tmp \
    && chown -R 1000:1000 /workspace /home/worker /tmp \
    && printf '#!/bin/sh\necho "$GIT_TOKEN"\n' > /bin/echo-token \
    && chmod +x /bin/echo-token

WORKDIR /

COPY --from=builder /workspace/worker /worker

USER 1000:1000
ENV HOME=/home/worker

ENTRYPOINT ["/worker"]
