FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/orka-mcp-e2e-server/ cmd/orka-mcp-e2e-server/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -a -ldflags="-s -w" -o /out/orka-mcp-e2e-server ./cmd/orka-mcp-e2e-server

FROM debian:bookworm-slim

COPY --from=builder /out/orka-mcp-e2e-server /orka-mcp-e2e-server

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libcap2-bin \
    && setcap 'cap_net_bind_service=+ep' /orka-mcp-e2e-server \
    && apt-get purge -y --auto-remove libcap2-bin \
    && rm -rf /var/lib/apt/lists/*

USER 65532:65532
ENV ORKA_WORKSPACE_AGENT_LISTEN_ADDR=:80
EXPOSE 80

ENTRYPOINT ["/orka-mcp-e2e-server"]
