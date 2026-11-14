# Build stage
FROM golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy source
COPY . .

# Build the worker
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -o worker ./workers/ai

# Runtime stage - using python slim for code_exec tool support
FROM python:3.14-slim

# Install Node.js for JavaScript execution
RUN apt-get update && apt-get install -y --no-install-recommends \
    nodejs \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN useradd -u 65532 -m nonroot

WORKDIR /

COPY --from=builder /workspace/worker /worker

# Create work directory for code execution
RUN mkdir -p /tmp/orka-exec && chown nonroot:nonroot /tmp/orka-exec

USER 65532:65532

ENTRYPOINT ["/worker"]
