FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.2-bookworm@sha256:47ce5636e9936b2c5cbf708925578ef386b4f8872aec74a67bd13a627d242b19 AS builder
ARG TARGETOS
ARG TARGETARCH
COPY scripts/fixtures/remote-mcp-gateway/main.go /src/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags='-s -w' -o /out/gateway /src/main.go
FROM scratch
COPY --from=builder /out/gateway /gateway
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/gateway"]
