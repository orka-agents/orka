FROM golang:1.26 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -o /live-kontxt-e2e ./scripts/live-kontxt-e2e

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /live-kontxt-e2e /live-kontxt-e2e
USER 65532:65532
ENTRYPOINT ["/live-kontxt-e2e"]
