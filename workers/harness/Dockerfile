# Build the CLI harness wrapper binary.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go run github.com/github/copilot-sdk/go/cmd/bundler \
    --platform linux/${TARGETARCH:-amd64} \
    --output cmd/orka-agent-harness-wrapper/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -a -o /out/orka-agent-harness-wrapper ./cmd/orka-agent-harness-wrapper
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -a -o /out/orka-workspace-agent ./cmd/orka-workspace-agent

FROM --platform=$TARGETPLATFORM golang:1.26 AS target-go

# Runtime stage with common CLI dependencies for generic, Codex, and Claude adapters.
FROM node:22-slim

# Keep the Codex CLI pinned until copilot-proxy accepts the internal
# metadata passthrough fields emitted by newer 0.142.x clients.
ARG CODEX_CLI_VERSION=0.141.0

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    build-essential \
    curl \
    git \
    libcap2-bin \
    make \
    pkg-config \
    ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @openai/codex@${CODEX_CLI_VERSION} @anthropic-ai/claude-code \
    && npm cache clean --force

COPY --from=target-go /usr/local/go /usr/local/go
RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go \
    && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
COPY --from=builder /out/orka-agent-harness-wrapper /orka-agent-harness-wrapper
COPY --from=builder /out/orka-workspace-agent /orka-workspace-agent

RUN printf '#!/bin/sh\necho "$GIT_TOKEN"\n' > /bin/echo-token \
    && chmod +x /bin/echo-token

RUN mkdir -p /workspace /home/node /tmp /app \
    && ln -s /home/node /home/worker \
    && chown -R 1000:1000 /workspace /home/node /tmp /app /orka-agent-harness-wrapper /orka-workspace-agent \
    && setcap 'cap_net_bind_service,cap_chown=+ep' /orka-workspace-agent

USER 1000:1000
ENV HOME=/home/worker
ENV PATH=/usr/local/go/bin:$PATH
ENV GOTOOLCHAIN=local
ENV ORKA_HARNESS_WRAPPER_RUNTIME=multi
WORKDIR /workspace

ENTRYPOINT ["/orka-agent-harness-wrapper"]
