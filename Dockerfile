# aegistudio/dex

# Build DeX executable using golang container
FROM golang:1.18.8-bullseye as builder

RUN mkdir -p /build

COPY . /build

RUN cd /build && \
    CGO_ENABLED=0 go build -ldflags="-extldflags=-static"  -o dex ./cmd/dex/

# Release the DeX container with TeXLive and TiKZ depenencies.
FROM aegistudio/dex-base:v1.0.0 as runner

RUN mkdir -p /app

COPY ./preset/ /app

COPY --from=builder /build/dex /app/dex

ENTRYPOINT ["/app/dex"]
