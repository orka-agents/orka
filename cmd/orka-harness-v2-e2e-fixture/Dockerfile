# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY internal/harness/v2/ internal/harness/v2/
COPY cmd/orka-harness-v2-e2e-fixture/ cmd/orka-harness-v2-e2e-fixture/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" \
    -o /out/orka-harness-v2-e2e-fixture ./cmd/orka-harness-v2-e2e-fixture

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/orka-harness-v2-e2e-fixture /orka-harness-v2-e2e-fixture

USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/orka-harness-v2-e2e-fixture"]
