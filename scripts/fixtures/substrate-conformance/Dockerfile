FROM golang:1.27.0 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY internal/ internal/
COPY pkg/ pkg/
COPY scripts/fixtures/substrate-conformance/ scripts/fixtures/substrate-conformance/
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/conformance ./scripts/fixtures/substrate-conformance
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/conformance /conformance
ENTRYPOINT ["/conformance"]
