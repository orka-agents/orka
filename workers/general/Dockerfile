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

# Runtime stage — alpine with common tools (sh, curl, etc.)
FROM alpine:3.22

RUN apk add --no-cache curl bash && \
    wget -q "https://dl.k8s.io/release/$(wget -qO- https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
      -O /usr/local/bin/kubectl && chmod +x /usr/local/bin/kubectl

WORKDIR /

COPY --from=builder /workspace/worker /worker

USER 65532:65532

ENTRYPOINT ["/worker"]
