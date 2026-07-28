FROM golang:1.26 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/orka-workspace-agent ./cmd/orka-workspace-agent

FROM debian:bookworm-slim

COPY --from=builder /out/orka-workspace-agent /orka-workspace-agent

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl libcap2-bin \
    && setcap 'cap_net_bind_service=+ep' /orka-workspace-agent \
    && apt-get purge -y --auto-remove libcap2-bin \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app /workspace /home/worker \
    && chmod 1777 /app /workspace /home/worker

# The workspace-agent is the PID-namespace supervisor. It runs as root so it can
# terminate/reap descendants, while every task command is dropped to the
# configured unprivileged UID/GID before exec.
USER 0:0
ENV HOME=/home/worker
WORKDIR /workspace
# Standalone image default matches the binary default and avoids privileged ports.
# ActorTemplates should still set this env explicitly so controller validation can
# compare it to the daemon port.
ENV ORKA_WORKSPACE_AGENT_LISTEN_ADDR=:8080
EXPOSE 80 8080

ENTRYPOINT ["/orka-workspace-agent"]
