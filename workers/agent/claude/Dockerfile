# Build the Go worker binary
FROM golang:1.26 AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build the worker
RUN CGO_ENABLED=0 GOOS=linux go build -a -o worker ./workers/agent/claude

# Runtime stage — Node.js required for Claude Code CLI
FROM node:22-slim

# Install git (needed for workspace cloning)
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install Claude Code CLI globally
RUN npm install -g @anthropic-ai/claude-code && npm cache clean --force

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
