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
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -a -o /out/orka-agent-harness-wrapper ./cmd/orka-agent-harness-wrapper

# Runtime stage with common CLI dependencies for generic, Codex, and Claude adapters.
FROM node:22-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    git \
    ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @openai/codex @anthropic-ai/claude-code \
    && npm cache clean --force

COPY --from=builder /out/orka-agent-harness-wrapper /orka-agent-harness-wrapper

RUN printf '#!/bin/sh\necho "$GIT_TOKEN"\n' > /bin/echo-token \
    && chmod +x /bin/echo-token

RUN mkdir -p /workspace /home/node /tmp \
    && ln -s /home/node /home/worker \
    && chown -R 1000:1000 /workspace /home/node /tmp /orka-agent-harness-wrapper

USER 1000:1000
ENV HOME=/home/worker
ENV ORKA_HARNESS_WRAPPER_RUNTIME=multi
WORKDIR /workspace

ENTRYPOINT ["/orka-agent-harness-wrapper"]
