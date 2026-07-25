# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY internal/gateway/protocol/ internal/gateway/protocol/
COPY internal/gateway/referenceadapter/ internal/gateway/referenceadapter/
COPY internal/redact/ internal/redact/
COPY cmd/orka-gateway-reference-adapter/ cmd/orka-gateway-reference-adapter/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" \
    -o /out/orka-gateway-reference-adapter ./cmd/orka-gateway-reference-adapter

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/orka-gateway-reference-adapter /orka-gateway-reference-adapter

USER 65532:65532
EXPOSE 8090

ENTRYPOINT ["/orka-gateway-reference-adapter"]
