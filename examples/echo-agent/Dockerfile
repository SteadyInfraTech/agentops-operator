# Build from the repository root:
#   docker build -f examples/echo-agent/Dockerfile -t echo-agent:dev .
FROM golang:1.24 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY examples/echo-agent/ examples/echo-agent/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /echo-agent ./examples/echo-agent

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /echo-agent /echo-agent
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/echo-agent"]
