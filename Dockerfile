FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod *.go ./
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 go build -o /outband .
RUN CGO_ENABLED=0 go build -o /mockllm ./cmd/mockllm

FROM gcr.io/distroless/static-debian12 AS proxy
COPY --from=builder /outband /outband
USER 65532
ENTRYPOINT ["/outband"]

FROM gcr.io/distroless/static-debian12 AS mockllm
COPY --from=builder /mockllm /mockllm
USER 65532
ENTRYPOINT ["/mockllm"]
