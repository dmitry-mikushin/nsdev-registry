#!/bin/sh
# init-cert.sh — generate a self-signed TLS keypair on first start when
# /etc/nsdev-registry/tls.{crt,key} are absent. Mounted by the systemd
# unit as ExecStartPre, idempotent, no-op when a real cert is already
# present.
#
# Why ship this at all: nsdev-registry REQUIRES TLS (HTTP/3 over QUIC
# has no plaintext mode). A fresh install with no PKI in place
# otherwise fails to start with "open /etc/nsdev-registry/tls.crt: no
# such file" which is a bad first impression. The self-signed cert is
# a placeholder — clients (nsdev-push) trust it via
# SkipServerVerification because identity is the ssh-issued bearer
# token, not PKI. Operators that want real cert validation drop in an
# ACME cert (certbot / lego / dehydrated) and remove or move
# tls.{crt,key} aside on schedule.

set -eu

CONF_DIR=/etc/nsdev-registry
CERT=$CONF_DIR/tls.crt
KEY=$CONF_DIR/tls.key

if [ -s "$CERT" ] && [ -s "$KEY" ]; then
    exit 0
fi

mkdir -p "$CONF_DIR"

# Build the SAN list. Always include localhost + 127.0.0.1; add the
# host's primary external address when `hostname -i` can resolve it,
# so podman pull from a cluster peer doesn't fail TLS name match with
# `--tls-verify=true`.
HOST_IP="$(hostname -i 2>/dev/null | awk '{print $1}' || true)"
SAN="DNS:localhost,IP:127.0.0.1"
if [ -n "$HOST_IP" ] && [ "$HOST_IP" != "127.0.0.1" ]; then
    SAN="$SAN,IP:$HOST_IP"
fi

umask 0177
openssl req -x509 -nodes \
    -newkey rsa:2048 \
    -keyout "$KEY" \
    -out "$CERT" \
    -days 365 \
    -subj "/CN=nsdev-registry" \
    -addext "subjectAltName=$SAN" \
    2>/dev/null

chmod 0640 "$CERT" "$KEY"
chown root:nsdev-registry "$CERT" "$KEY" 2>/dev/null || true
