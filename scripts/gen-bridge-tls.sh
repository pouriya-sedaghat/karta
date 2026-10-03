#!/bin/sh
# Creates the private TLS material of a co-located controlled source bridge
# (never overwrites an existing file):
#
#   secrets/bridge/ca-key.pem, ca.pem   a CA for this deployment's bridge only
#   secrets/bridge/tls-key.pem, tls.pem bridge-serve's key and certificate,
#                                       for the name bridge-serve plus
#                                       BRIDGE_TLS_NAMES (space-separated DNS
#                                       names or IPv4 addresses of a separate
#                                       bridge host)
#   config/sources/bridge-ca.pem        a copy of ca.pem (git-ignored): the
#                                       fetcher's source file trusts only it
#                                       (ca_file)
#
# The CA key never leaves secrets/bridge and is not mounted anywhere. To renew
# the server certificate, move tls.pem and tls-key.pem away and run again; to
# replace the CA, move the whole directory away (and update the fetcher).
# BRIDGE_TLS_DAYS sets the server certificate's validity (default 397).
set -eu
cd "$(dirname "$0")/.."
umask 077
d=secrets/bridge
mkdir -p "$d"
chmod 700 secrets "$d"
if [ ! -s "$d/ca-key.pem" ]; then
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$d/ca-key.pem" 2> /dev/null
  openssl req -x509 -new -key "$d/ca-key.pem" -sha256 -days 3650 -subj "/CN=Karta bridge CA (this deployment only)" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" -addext "keyUsage=critical,keyCertSign,cRLSign" -out "$d/ca.pem"
  echo "created $d/ca.pem"
fi
if [ ! -s "$d/tls.pem" ]; then
  san="DNS:bridge-serve"
  for n in ${BRIDGE_TLS_NAMES:-}; do
    case "$n" in
      *[!0-9.]*) san="$san,DNS:$n" ;;
      *) san="$san,IP:$n" ;;
    esac
  done
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$d/tls-key.pem" 2> /dev/null
  openssl req -new -key "$d/tls-key.pem" -subj "/CN=bridge-serve" -out "$d/tls.csr"
  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\nsubjectAltName=%s\n' "$san" > "$d/tls.ext"
  openssl x509 -req -in "$d/tls.csr" -CA "$d/ca.pem" -CAkey "$d/ca-key.pem" -CAcreateserial -sha256 \
    -days "${BRIDGE_TLS_DAYS:-397}" -extfile "$d/tls.ext" -out "$d/tls.pem" 2> /dev/null
  rm -f "$d/tls.csr" "$d/tls.ext"
  echo "created $d/tls.pem for $san"
fi
# Readable by bridge-serve (UID 65532) through the bind mount; the
# directories stay private (0700).
chmod 644 "$d/ca.pem" "$d/tls.pem" "$d/tls-key.pem"
if ! cmp -s "$d/ca.pem" config/sources/bridge-ca.pem 2> /dev/null; then
  cp "$d/ca.pem" config/sources/bridge-ca.pem
  chmod 644 config/sources/bridge-ca.pem
  echo "wrote config/sources/bridge-ca.pem"
fi
