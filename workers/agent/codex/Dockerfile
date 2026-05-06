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

# Install git (needed for workspace cloning)
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    ca-certificates \
    curl \
    ripgrep \
    make \
    build-essential \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Reuse the current Go toolchain from the builder image so runtime validation
# can handle repositories that require modern Go versions. Codex executes
# commands through a login shell whose PATH may be reset, so expose the common
# Go entrypoints from /usr/local/bin as well as setting PATH on the image.
COPY --from=builder /usr/local/go /usr/local/go
RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go     && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
ENV GOROOT=/usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

# Install the published Codex CLI package. It resolves the correct optional
# platform dependency for the target linux architecture during install.
RUN npm install -g @openai/codex \
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
