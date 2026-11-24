# syntax=docker/dockerfile:1
# rpcgate container image. See README.md.
#
#   docker build -t rpcgate .                                 # this machine's platform
#   docker buildx build --platform linux/amd64 -t rpcgate --load .

# The Go build runs on the builder's native platform and cross-compiles for
# the target, so nothing compiles under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
# Pure Go (PocketBase uses a CGo-free SQLite), so the binary is static.
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/rpcgate ./cmd/rpcgate

# Alpine for convenience: a shell and busybox tools for debugging
# (`docker compose exec rpcgate sh`). Its base already has CA certificates.
# The data directory belongs to nobody, so a named volume mounted there starts
# out writable.
FROM alpine:3
RUN mkdir /pb_data && chown nobody:nobody /pb_data
COPY --from=build /out/rpcgate /rpcgate
USER nobody:nobody
ENV RPCGATE_DATA_DIR=/pb_data \
    RPCGATE_LISTEN=0.0.0.0:8545
VOLUME /pb_data
# One port: the proxy at /<token>/<chain>, PocketBase's admin UI at /_/.
EXPOSE 8545
ENTRYPOINT ["/rpcgate"]
