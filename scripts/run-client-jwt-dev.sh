#!/usr/bin/env bash

set -euo pipefail

if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required for JWT generation" >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

CLIENT_ID="${CLIENT_ID:-client-a}"
LOCAL_TARGET="${LOCAL_TARGET:-127.0.0.1:3000}"
SERVER_URL="${SERVER_URL:-ws://127.0.0.1:8080/ws}"
JWT_AUDIENCE="${JWT_AUDIENCE:-burrow-server}"
JWT_ISSUER="${JWT_ISSUER:-dev-local}"
JWT_SECRET="${JWT_HMAC_SECRET:-${JWT_SECRET:-dev-secret}}"
JWT_TTL_SECONDS="${JWT_TTL_SECONDS:-300}"

if [[ -z "${JWT_SECRET}" ]]; then
  echo "JWT secret must be set via JWT_HMAC_SECRET or JWT_SECRET" >&2
  exit 1
fi

BEARER_TOKEN="$(python3 - "$CLIENT_ID" "$JWT_AUDIENCE" "$JWT_ISSUER" "$JWT_SECRET" "$JWT_TTL_SECONDS" <<'PY'
import base64
import hashlib
import hmac
import json
import sys
import time

client_id = sys.argv[1]
aud = sys.argv[2]
iss = sys.argv[3]
secret = sys.argv[4].encode("utf-8")
ttl = int(sys.argv[5])
now = int(time.time())

header = {"alg": "HS256", "typ": "JWT"}
payload = {
    "iss": iss,
    "aud": [aud],
    "sub": client_id,
    "iat": now,
    "nbf": now - 5,
    "exp": now + ttl,
}

def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")

segments = [
    b64url(json.dumps(header, separators=(",", ":")).encode("utf-8")),
    b64url(json.dumps(payload, separators=(",", ":")).encode("utf-8")),
]
signing_input = ".".join(segments).encode("ascii")
sig = hmac.new(secret, signing_input, hashlib.sha256).digest()
token = ".".join([segments[0], segments[1], b64url(sig)])
print(token)
PY
)"

echo "Starting JWT client with sub/client_id=${CLIENT_ID}, aud=${JWT_AUDIENCE}, ttl=${JWT_TTL_SECONDS}s"

cd "$ROOT_DIR"
BURROW_JWT_ALG=HS256 \
BURROW_JWT_HMAC_SECRET="$JWT_SECRET" \
BURROW_JWT_ISSUER="$JWT_ISSUER" \
BURROW_JWT_AUDIENCE="$JWT_AUDIENCE" \
BURROW_BEARER_TOKEN="$BEARER_TOKEN" \
BURROW_CLIENT_ID="$CLIENT_ID" \
BURROW_LOCAL_TARGET="$LOCAL_TARGET" \
BURROW_SERVER_URL="$SERVER_URL" \
go run ./cmd/root client