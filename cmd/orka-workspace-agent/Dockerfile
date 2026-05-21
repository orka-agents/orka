FROM golang:1.26 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/orka-workspace-agent ./cmd/orka-workspace-agent

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app /workspace /home/worker \
    && chmod 1777 /app /workspace /home/worker

COPY --from=builder /out/orka-workspace-agent /orka-workspace-agent

USER 1000:1000
WORKDIR /workspace
# Standalone image default matches the binary default and avoids privileged ports.
# ActorTemplates should still set this env explicitly so controller validation can
# compare it to the daemon port.
ENV ORKA_WORKSPACE_AGENT_LISTEN_ADDR=:8080
EXPOSE 8080

ENTRYPOINT ["/orka-workspace-agent"]
