#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required for e2e smoke tests" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required for e2e smoke tests" >&2
  exit 1
fi
if ! command -v nc >/dev/null 2>&1; then
  echo "nc is required for e2e smoke tests" >&2
  exit 1
fi

reserve_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

wait_for_http_ok() {
  local url="$1"
  local attempts="${2:-80}"
  local i
  for ((i = 1; i <= attempts; i++)); do
    if curl -fsS "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

wait_for_tcp() {
  local host="$1"
  local port="$2"
  local attempts="${3:-80}"
  local i
  for ((i = 1; i <= attempts; i++)); do
    if nc -z "$host" "$port" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "assert failed: expected output to contain '$needle'" >&2
    exit 1
  fi
}

send_payload() {
  local payload="$1"
  local bridge_port="$2"
  printf "%s" "$payload" | nc -w 2 127.0.0.1 "$bridge_port" || true
}

mint_bearer_token() {
  local client_id="$1"
  local aud="$2"
  local iss="$3"
  local secret="$4"
  local ttl_seconds="${5:-300}"
  python3 - "$client_id" "$aud" "$iss" "$secret" "$ttl_seconds" <<'PY'
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
print(".".join([segments[0], segments[1], b64url(sig)]))
PY
}

wait_for_tunnel_echo() {
  local payload="$1"
  local bridge_port="$2"
  local attempts="${3:-40}"
  local i
  for ((i = 1; i <= attempts; i++)); do
    local out
    out="$(send_payload "$payload" "$bridge_port")"
    if [[ "$out" == "$payload" ]]; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

wait_for_tunnel_down() {
  local payload="$1"
  local bridge_port="$2"
  local attempts="${3:-40}"
  local i
  for ((i = 1; i <= attempts; i++)); do
    local out
    out="$(send_payload "$payload" "$bridge_port")"
    if [[ "$out" != "$payload" ]]; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

metric_value() {
  local metrics_url="$1"
  local name="$2"
  curl -fsS "$metrics_url" | awk -v metric="$name" '$1==metric {print $2; exit}'
}

wait_for_metric_equals() {
  local metrics_url="$1"
  local name="$2"
  local want="$3"
  local attempts="${4:-60}"
  local i
  for ((i = 1; i <= attempts; i++)); do
    local got
    got="$(metric_value "$metrics_url" "$name" || true)"
    if [[ "$got" == "$want" ]]; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

cleanup() {
  set +e
  [[ -n "${CLIENT_PID:-}" ]] && kill_tree "$CLIENT_PID" || true
  [[ -n "${SERVER_PID:-}" ]] && kill_tree "$SERVER_PID" || true
  [[ -n "${ECHO_PID:-}" ]] && kill "$ECHO_PID" >/dev/null 2>&1 || true
  wait >/dev/null 2>&1 || true
}
trap cleanup EXIT

kill_tree() {
  local pid="$1"
  if [[ -z "$pid" ]]; then
    return 0
  fi
  pkill -TERM -P "$pid" >/dev/null 2>&1 || true
  kill -TERM "$pid" >/dev/null 2>&1 || true
}

JWT_SECRET="${JWT_SECRET:-dev-secret}"
JWT_AUDIENCE="${JWT_AUDIENCE:-krt-server}"
JWT_ISSUER="${JWT_ISSUER:-dev-local}"
SERVER_PORT="${SERVER_PORT:-$(reserve_port)}"
BRIDGE_PORT="${BRIDGE_PORT:-$(reserve_port)}"
TARGET_PORT="${TARGET_PORT:-$(reserve_port)}"
CLIENT_ID="${CLIENT_ID:-client-e2e}"

SERVER_ADDR="127.0.0.1:${SERVER_PORT}"
BRIDGE_ADDR="127.0.0.1:${BRIDGE_PORT}"
SERVER_URL="ws://127.0.0.1:${SERVER_PORT}/ws"
LOCAL_TARGET="127.0.0.1:${TARGET_PORT}"
CLIENT_BEARER_TOKEN="$(mint_bearer_token "$CLIENT_ID" "$JWT_AUDIENCE" "$JWT_ISSUER" "$JWT_SECRET" 300)"

echo "[e2e] starting local tcp echo target on ${LOCAL_TARGET}"
python3 - <<PY >/tmp/krt-e2e-echo.log 2>&1 &
import socket
import threading

HOST = "127.0.0.1"
PORT = ${TARGET_PORT}

srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind((HOST, PORT))
srv.listen(64)

def handle(conn):
    try:
        while True:
            data = conn.recv(65535)
            if not data:
                break
            conn.sendall(data)
    finally:
        conn.close()

while True:
    conn, _ = srv.accept()
    t = threading.Thread(target=handle, args=(conn,), daemon=True)
    t.start()
PY
ECHO_PID=$!

wait_for_tcp 127.0.0.1 "$TARGET_PORT" 80 || {
  echo "[e2e] echo target did not start" >&2
  exit 1
}

echo "[e2e] starting tunnel server on ${SERVER_ADDR} (bridge ${BRIDGE_ADDR})"
(
  cd "$ROOT_DIR"
  KRT_JWT_ALG="HS256" \
  KRT_JWT_HMAC_SECRET="$JWT_SECRET" \
  KRT_JWT_ISSUER="$JWT_ISSUER" \
  KRT_JWT_AUDIENCE="$JWT_AUDIENCE" \
  KRT_SERVER_ADDR="$SERVER_ADDR" \
  KRT_BRIDGE_ADDR="$BRIDGE_ADDR" \
  exec go run ./cmd/root server
) >/tmp/krt-e2e-server.log 2>&1 &
SERVER_PID=$!

wait_for_http_ok "http://127.0.0.1:${SERVER_PORT}/healthz" 100 || {
  echo "[e2e] server health check failed" >&2
  exit 1
}

echo "[e2e] starting tunnel client to ${SERVER_URL}"
(
  cd "$ROOT_DIR"
  KRT_JWT_ALG="HS256" \
  KRT_JWT_HMAC_SECRET="$JWT_SECRET" \
  KRT_JWT_ISSUER="$JWT_ISSUER" \
  KRT_JWT_AUDIENCE="$JWT_AUDIENCE" \
  KRT_BEARER_TOKEN="$CLIENT_BEARER_TOKEN" \
  KRT_SERVER_URL="$SERVER_URL" \
  KRT_CLIENT_ID="$CLIENT_ID" \
  KRT_LOCAL_TARGET="$LOCAL_TARGET" \
  exec go run ./cmd/root client
) >/tmp/krt-e2e-client.log 2>&1 &
CLIENT_PID=$!

wait_for_tcp 127.0.0.1 "$BRIDGE_PORT" 100 || {
  echo "[e2e] bridge listener not reachable" >&2
  exit 1
}

echo "[e2e] validating /metrics endpoint"
metrics="$(curl -fsS "http://127.0.0.1:${SERVER_PORT}/metrics")"
METRICS_URL="http://127.0.0.1:${SERVER_PORT}/metrics"
assert_contains "$metrics" "krt_sessions_active"
assert_contains "$metrics" "krt_streams_active"
assert_contains "$metrics" "krt_stale_services_deleted_total"

echo "[e2e] validating tunnel data path"
if ! wait_for_tunnel_echo "smoke-ok" "$BRIDGE_PORT" 60; then
  echo "[e2e] tunnel echo check failed" >&2
  exit 1
fi

echo "[e2e] simulating client failure"
kill_tree "$CLIENT_PID"
wait "$CLIENT_PID" >/dev/null 2>&1 || true
CLIENT_PID=""
sleep 0.5

echo "[e2e] ensuring bridge does not pass traffic while client is down"
if ! wait_for_tunnel_down "should-fail" "$BRIDGE_PORT" 40; then
  echo "[e2e] expected no echo while client is down" >&2
  exit 1
fi

echo "[e2e] restarting client to validate recovery"
CLIENT_BEARER_TOKEN="$(mint_bearer_token "$CLIENT_ID" "$JWT_AUDIENCE" "$JWT_ISSUER" "$JWT_SECRET" 300)"
(
  cd "$ROOT_DIR"
  KRT_JWT_ALG="HS256" \
  KRT_JWT_HMAC_SECRET="$JWT_SECRET" \
  KRT_JWT_ISSUER="$JWT_ISSUER" \
  KRT_JWT_AUDIENCE="$JWT_AUDIENCE" \
  KRT_BEARER_TOKEN="$CLIENT_BEARER_TOKEN" \
  KRT_SERVER_URL="$SERVER_URL" \
  KRT_CLIENT_ID="$CLIENT_ID" \
  KRT_LOCAL_TARGET="$LOCAL_TARGET" \
  exec go run ./cmd/root client
) >/tmp/krt-e2e-client.log 2>&1 &
CLIENT_PID=$!

if ! wait_for_tunnel_echo "recover-ok" "$BRIDGE_PORT" 80; then
  echo "[e2e] reconnect recovery check failed" >&2
  exit 1
fi

echo "[e2e] success: smoke and failure/recovery checks passed"
