# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build the worker
RUN CGO_ENABLED=0 GOOS=linux go build -a -o worker ./workers/general

# Runtime stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/worker /worker

USER 65532:65532

ENTRYPOINT ["/worker"]
