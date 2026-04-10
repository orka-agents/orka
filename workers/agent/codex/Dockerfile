# Build the Go worker binary
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build the worker
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -a -o worker ./workers/agent/codex

# Runtime stage — Node.js required for Codex CLI
FROM node:22-slim

ARG TARGETARCH

# Install git (needed for workspace cloning)
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install Codex CLI globally with the matching platform package
RUN arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
    && case "$arch" in \
        amd64) codex_pkg='@openai/codex-linux-x64' ;; \
        arm64) codex_pkg='@openai/codex-linux-arm64' ;; \
        *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
      esac \
    && npm install -g @openai/codex "$codex_pkg" \
    && npm cache clean --force

# Create a simple token-echo script for git auth
RUN printf '#!/bin/sh\necho "$GIT_TOKEN"\n' > /bin/echo-token \
    && chmod +x /bin/echo-token

# Copy the Go worker binary
COPY --from=builder /workspace/worker /worker

# Create writable directories for readOnlyRootFilesystem compatibility
# node:22-slim already has uid 1000 as the 'node' user
RUN mkdir -p /workspace /home/node /tmp \
    && ln -s /home/node /home/worker \
    && chown -R 1000:1000 /workspace /home/node /tmp

USER 1000:1000
ENV HOME=/home/worker

ENTRYPOINT ["/worker"]
