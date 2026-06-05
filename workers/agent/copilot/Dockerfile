# Build the Go worker binary
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Bundle the Copilot CLI binary (downloads and embeds the platform-specific CLI)
RUN go run github.com/github/copilot-sdk/go/cmd/bundler \
    --platform linux/${TARGETARCH:-amd64} \
    --output workers/agent/copilot/

# Build the worker and workspace daemon. The worker embeds the bundled CLI.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -a -o worker ./workers/agent/copilot
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -a -o orka-workspace-agent ./cmd/orka-workspace-agent

# Runtime stage — minimal base (bundled CLI is a standalone native binary)
FROM debian:bookworm-slim

# Install git (needed for workspace cloning)
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    curl \
    ca-certificates \
    libcap2-bin \
    && rm -rf /var/lib/apt/lists/*

# Create a simple token-echo script for git auth
RUN printf '#!/bin/sh\necho "$GIT_TOKEN"\n' > /bin/echo-token \
    && chmod +x /bin/echo-token

# Copy the Go worker binaries
COPY --from=builder /workspace/worker /worker
COPY --from=builder /workspace/orka-workspace-agent /orka-workspace-agent

RUN setcap 'cap_net_bind_service=+ep' /orka-workspace-agent \
    && apt-get purge -y --auto-remove libcap2-bin

# Create writable directories for readOnlyRootFilesystem compatibility.
# The embedded CLI is extracted to a cache dir at runtime.
RUN groupadd -g 1000 worker && useradd -u 1000 -g worker -m worker \
    && mkdir -p /app /workspace /tmp \
    && chown -R 1000:1000 /app /workspace /home/worker /tmp

USER 1000:1000
ENV HOME=/home/worker

ENTRYPOINT ["/worker"]
