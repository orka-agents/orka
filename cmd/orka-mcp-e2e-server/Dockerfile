FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/orka-mcp-e2e-server/ cmd/orka-mcp-e2e-server/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -a -ldflags="-s -w" -o /out/orka-mcp-e2e-server ./cmd/orka-mcp-e2e-server

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/orka-mcp-e2e-server /orka-mcp-e2e-server

USER 65532:65532
ENV ORKA_WORKSPACE_AGENT_LISTEN_ADDR=:8080
EXPOSE 8080

ENTRYPOINT ["/orka-mcp-e2e-server"]
