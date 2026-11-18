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
    CGO_ENABLED=0 GOOS=linux go build -o worker ./workers/general \
    && cc -std=c11 -O2 -Wall -Wextra -Werror -static \
      -fstack-protector-strong -D_FORTIFY_SOURCE=2 -Wl,-z,relro,-z,now \
      -o validation-git-launcher workers/general/gitlauncher/launcher.c

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
COPY --from=builder --chmod=0555 /workspace/validation-git-launcher /usr/local/libexec/orka-validation-git

USER 1000:1000
ENV HOME=/home/worker

ENTRYPOINT ["/worker"]
