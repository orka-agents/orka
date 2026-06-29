# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/orka-example-echo-harness ./examples/harness/echo

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/orka-example-echo-harness /orka-example-echo-harness
USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/orka-example-echo-harness"]
