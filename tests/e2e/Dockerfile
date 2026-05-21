# Containerised build of nsdev-registry for the E2E test harness.
#
# Multi-stage so we don't ship the Go toolchain in the final image —
# the e2e tests don't care about size but keeping the image lean makes
# `docker run` start times tighter, and tightens the feedback loop the
# tests are designed to provide.
#
# The build stage compiles from the working tree (mounted as the
# build context), so a developer iterating on registry/quicserver.go
# can run the e2e suite without pushing anything: the next
# `docker build` picks up their unstaged changes.

FROM golang:1.26-alpine AS build

WORKDIR /src

# Pre-copy go.mod/go.sum/vendor so the Go module download layer
# caches independently of the source tree.
COPY go.mod go.sum ./
COPY vendor/ ./vendor/

# Now the source. The build is mod=vendor (vendor/ is committed).
COPY . .

RUN GO111MODULE=on CGO_ENABLED=0 go build \
    -mod=vendor \
    -ldflags='-s -w' \
    -o /out/nsdev-registry \
    ./cmd/registry

# -- Runtime image ---------------------------------------------------
#
# Alpine + curl (HTTP/2 probe; --http3-only is in our test image only
# via the python-h3 client). OpenSSL for self-signed cert generation
# on container start. Tini for clean signal handling so `docker stop`
# doesn't leave an orphan listener.

FROM alpine:3.21

RUN apk add --no-cache \
    ca-certificates \
    openssl \
    curl \
    tini

COPY --from=build /out/nsdev-registry /usr/local/bin/nsdev-registry

# Self-signed TLS material baked at container start. The e2e harness
# trusts any cert (clients use --insecure / SkipServerVerification),
# so the cert is purely a TLS-stack requirement, not a security
# control. Identity in the e2e protocol is the bearer token issued
# through /v3/sessions.
COPY tests/e2e/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Storage stays inside the container — every e2e scenario starts a
# fresh container so there's no benefit to a volume mount.
RUN mkdir -p /var/lib/nsdev-registry

EXPOSE 5000/tcp 5000/udp

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
