# syntax=docker/dockerfile:1
# Test-only: never substitute this image for a production worker.
FROM --platform=$BUILDPLATFORM golang:1.27.0 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
COPY cmd/orka-gateway-e2e-worker/ cmd/orka-gateway-e2e-worker/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/orka-gateway-e2e-worker
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/worker /worker
USER 1000:1000
ENTRYPOINT ["/worker"]
