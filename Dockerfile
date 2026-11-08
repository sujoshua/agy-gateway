# syntax=docker/dockerfile:1
#
# The official runtime is not redistributable from this repository, so the
# default image does not contain it. On first start the gateway downloads the
# official archive from Google, checks its pinned SHA-256 and unpacks it under
# /data/.cache/agy-gateway/runtime (about 1 GB). To use a runtime of your own
# instead, mount the unpacked directory (agy_acp_server.par and
# localharness_external, which the runtime finds next to itself) read-only at
# /opt/agy; it takes precedence. To bake it in for a private registry, put both
# files in runtime/ and build with --target bundled.

# The build stage runs on the build host and cross-compiles, so multi-arch
# builds do not compile Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/agy-gateway ./cmd/agy-gateway

# The runtime is a dynamically linked glibc binary that calls Google APIs over
# TLS, so the base needs glibc and CA certificates. tini reaps the runtime's
# child processes. The work directory is world-writable (sticky) so the image
# also runs under an arbitrary uid.
FROM debian:bookworm-slim AS base
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tini \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home --home-dir /data agy \
    && mkdir -p /opt/agy /tmp/agy-gateway \
    && chmod 1777 /tmp/agy-gateway
COPY --from=build /out/agy-gateway /usr/local/bin/agy-gateway
# Runtime state (sign-in, conversations) lives under
# /data/.gemini/antigravity-acp; mount a volume at /data to keep it.
ENV AGY_GW_LISTEN=:8080 \
    AGY_GW_RUNTIME_CMD=/opt/agy/agy_acp_server.par \
    AGY_GW_RUNTIME_DOWNLOAD=true \
    AGY_GW_RUNTIME_HOME=/data \
    AGY_GW_WORKDIR=/tmp/agy-gateway
USER agy
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/agy-gateway"]
CMD ["serve"]

FROM base AS bundled
COPY --chmod=0555 runtime/agy_acp_server.par runtime/localharness_external /opt/agy/

# Default target: the gateway without the runtime.
FROM base AS gateway
