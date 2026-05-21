#!/bin/sh
# Container entrypoint: generate a fresh self-signed TLS keypair, write
# a minimal nsdev-registry config that wires TCP + QUIC on the same
# port, then exec the binary as PID 1 (under tini, so signals reach it).
#
# The cert is regenerated on every container start so two consecutive
# `docker run` invocations don't accidentally share key material —
# this matches how the e2e harness expects each scenario to be a
# completely fresh process.

set -eu

CERT_DIR=${CERT_DIR:-/etc/nsdev-registry}
CERT=$CERT_DIR/tls.crt
KEY=$CERT_DIR/tls.key
CONFIG=$CERT_DIR/config.yml

mkdir -p "$CERT_DIR"

if [ ! -s "$CERT" ] || [ ! -s "$KEY" ]; then
    openssl req -x509 -nodes \
        -newkey rsa:2048 \
        -keyout "$KEY" \
        -out "$CERT" \
        -days 7 \
        -subj "/CN=nsdev-registry-e2e" \
        -addext "subjectAltName=DNS:localhost,DNS:nsdev-registry,IP:127.0.0.1,IP:0.0.0.0" \
        2>/dev/null
fi

cat > "$CONFIG" <<EOF
version: 0.1
log:
  level: info
storage:
  filesystem:
    rootdirectory: /var/lib/nsdev-registry
http:
  addr: 0.0.0.0:5000
  tls:
    certificate: $CERT
    key: $KEY
  quic:
    addr: 0.0.0.0:5000
EOF

exec /usr/local/bin/nsdev-registry serve "$CONFIG"
