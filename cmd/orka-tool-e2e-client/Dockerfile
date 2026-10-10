FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS builder

ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY internal/ internal/
COPY cmd/orka-tool-e2e-client/ cmd/orka-tool-e2e-client/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -a -ldflags="-s -w" -o /out/orka-tool-e2e-client ./cmd/orka-tool-e2e-client

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/orka-tool-e2e-client /orka-tool-e2e-client

USER 65532:65532

ENTRYPOINT ["/orka-tool-e2e-client"]
