#!/usr/bin/env sh
# Regenerates the dev-only east-west mTLS CA + shared leaf cert used by
# playground/docker-compose.yml (see README.md in this directory).
set -eu
cd "$(dirname "$0")"

rm -f ca.key ca.crt ca.srl server.key server.crt server.csr server.ext

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout ca.key -out ca.crt -days 3650 \
  -subj "/CN=kyuusha-dev-ca"

cat > server.ext <<'EOF'
subjectAltName = DNS:api-gateway,DNS:compute,DNS:identity,DNS:image,DNS:network,DNS:block-storage,DNS:compute-agent-1,DNS:compute-agent-2,DNS:compute-agent-3,DNS:localhost,IP:127.0.0.1
EOF

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout server.key -out server.csr \
  -subj "/CN=kyuusha-internal"

openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days 3650 -extfile server.ext

rm -f server.csr server.ext ca.srl ca.key
