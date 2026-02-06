# Build stage
FROM golang:1.25 AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build the worker
RUN CGO_ENABLED=0 GOOS=linux go build -a -o worker ./workers/general

# Runtime stage
FROM bitnami/kubectl:latest AS kubectl

FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/worker /worker
COPY --from=kubectl /opt/bitnami/kubectl/bin/kubectl /usr/local/bin/kubectl

USER 65532:65532

ENTRYPOINT ["/worker"]
