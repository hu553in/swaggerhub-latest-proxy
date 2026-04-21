# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26.2
ARG DEBIAN_VERSION=bookworm

FROM golang:${GO_VERSION}-${DEBIAN_VERSION} AS builder
WORKDIR /src

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod \
  go mod download

COPY cmd/ ./cmd/

RUN --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=0 GOFLAGS="-buildvcs=false" \
  go build -trimpath -ldflags="-s -w" -o /dist/swaggerhub-latest-proxy ./cmd/swaggerhub-latest-proxy

FROM debian:${DEBIAN_VERSION}-slim AS runner

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl \
  && rm -rf /var/lib/apt/lists/*

RUN groupadd --gid 10001 app \
  && useradd --uid 10001 --gid 10001 -M app

COPY --from=builder /dist/swaggerhub-latest-proxy /usr/local/bin/swaggerhub-latest-proxy

USER app
ENTRYPOINT ["/usr/local/bin/swaggerhub-latest-proxy"]
