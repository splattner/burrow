#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# ---------------------------------------------------------------------------
# Dependency checks
# ---------------------------------------------------------------------------

for cmd in python3 curl nc; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "$cmd is required for e2e smoke tests" >&2
    exit 1
  fi
done

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

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

assert_not_contains() {
  local haystack="$1"
  local needle="$2"
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "assert failed: expected output NOT to contain '$needle'" >&2
    exit 1
  fi
}

send_payload() {
  local payload="$1"
  local addr="$2"           # host:port or just port (127.0.0.1 assumed)
  local host port
  if [[ "$addr" == *:* ]]; then
    host="${addr%%:*}"
    port="${addr##*:}"
  else
    host="127.0.0.1"
    port="$addr"
  fi
  printf "%s" "$payload" | nc -w 2 "$host" "$port" || true
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

# Returns the bridge address for a connected client from the server API.
get_bridge_addr() {
  local server_port="$1"
  local client_id="$2"
  curl -fsS "http://127.0.0.1:${server_port}/api/clients/${client_id}/bridge-addr" 2>/dev/null || true
}

# Polls until the server exposes a bridge address for client_id, then
# prints it and returns 0. Returns 1 on timeout.
wait_for_bridge_addr() {
  local server_port="$1"
  local client_id="$2"
  local attempts="${3:-80}"
  local i addr
  for ((i = 1; i <= attempts; i++)); do
    addr="$(get_bridge_addr "$server_port" "$client_id")"
    if [[ -n "$addr" ]]; then
      echo "$addr"
      return 0
    fi
    sleep 0.1
  done
  return 1
}

wait_for_tunnel_echo() {
  local payload="$1"
  local bridge_addr="$2"
  local attempts="${3:-40}"
  local i
  for ((i = 1; i <= attempts; i++)); do
    local out
    out="$(send_payload "$payload" "$bridge_addr")"
    if [[ "$out" == "$payload" ]]; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

# Returns 0 once the bridge no longer echoes the payload (client disconnected).
wait_for_tunnel_down() {
  local payload="$1"
  local bridge_addr="$2"
  local attempts="${3:-40}"
  local i
  for ((i = 1; i <= attempts; i++)); do
    local out
    out="$(send_payload "$payload" "$bridge_addr")"
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

start_echo_server() {
  local port="$1"
  python3 - <<PY >/tmp/burrow-e2e-echo-${port}.log 2>&1 &
import socket, threading

srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", ${port}))
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
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
PY
  echo $!
}

start_client() {
  local client_id="$1"
  local server_url="$2"
  local local_target="$3"
  local token="$4"
  local log_suffix="${5:-$client_id}"
  (
    cd "$ROOT_DIR"
    BURROW_JWT_ALG="HS256" \
    BURROW_JWT_HMAC_SECRET="$JWT_SECRET" \
    BURROW_JWT_ISSUER="$JWT_ISSUER" \
    BURROW_JWT_AUDIENCE="$JWT_AUDIENCE" \
    BURROW_BEARER_TOKEN="$token" \
    BURROW_SERVER_URL="$server_url" \
    BURROW_CLIENT_ID="$client_id" \
    BURROW_LOCAL_TARGET="$local_target" \
    exec go run ./cmd/root client
  ) >"/tmp/burrow-e2e-client-${log_suffix}.log" 2>&1 &
  echo $!
}

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------

cleanup() {
  set +e
  [[ -n "${CLIENT_PID:-}"   ]] && kill_tree "$CLIENT_PID"   || true
  [[ -n "${CLIENT_B_PID:-}" ]] && kill_tree "$CLIENT_B_PID" || true
  [[ -n "${SERVER_PID:-}"   ]] && kill_tree "$SERVER_PID"   || true
  [[ -n "${ECHO_PID:-}"     ]] && kill "$ECHO_PID"  >/dev/null 2>&1 || true
  [[ -n "${ECHO_B_PID:-}"   ]] && kill "$ECHO_B_PID" >/dev/null 2>&1 || true
  wait >/dev/null 2>&1 || true
}
trap cleanup EXIT

kill_tree() {
  local pid="$1"
  [[ -z "$pid" ]] && return 0
  pkill -TERM -P "$pid" >/dev/null 2>&1 || true
  kill -TERM  "$pid"    >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

JWT_SECRET="${JWT_SECRET:-dev-secret}"
JWT_AUDIENCE="${JWT_AUDIENCE:-burrow-server}"
JWT_ISSUER="${JWT_ISSUER:-dev-local}"
SERVER_PORT="${SERVER_PORT:-$(reserve_port)}"
TARGET_PORT="${TARGET_PORT:-$(reserve_port)}"
TARGET_PORT_B="${TARGET_PORT_B:-$(reserve_port)}"
CLIENT_ID="${CLIENT_ID:-client-e2e}"
CLIENT_ID_B="${CLIENT_ID_B:-client-e2e-b}"

SERVER_ADDR="127.0.0.1:${SERVER_PORT}"
# Bridge addr uses host-only (port 0 = random per client)
BRIDGE_BIND="127.0.0.1"
SERVER_URL="ws://127.0.0.1:${SERVER_PORT}/ws"
METRICS_URL="http://127.0.0.1:${SERVER_PORT}/metrics"
LOCAL_TARGET="127.0.0.1:${TARGET_PORT}"
LOCAL_TARGET_B="127.0.0.1:${TARGET_PORT_B}"

CLIENT_PID=""
CLIENT_B_PID=""
SERVER_PID=""
ECHO_PID=""
ECHO_B_PID=""

# ---------------------------------------------------------------------------
# Start infrastructure
# ---------------------------------------------------------------------------

echo "[e2e] starting echo server A on ${LOCAL_TARGET}"
ECHO_PID="$(start_echo_server "$TARGET_PORT")"
wait_for_tcp 127.0.0.1 "$TARGET_PORT" 80 || { echo "[e2e] echo server A did not start" >&2; exit 1; }

echo "[e2e] starting echo server B on ${LOCAL_TARGET_B}"
ECHO_B_PID="$(start_echo_server "$TARGET_PORT_B")"
wait_for_tcp 127.0.0.1 "$TARGET_PORT_B" 80 || { echo "[e2e] echo server B did not start" >&2; exit 1; }

echo "[e2e] starting tunnel server on ${SERVER_ADDR} (bridge bind ${BRIDGE_BIND})"
(
  cd "$ROOT_DIR"
  BURROW_JWT_ALG="HS256" \
  BURROW_JWT_HMAC_SECRET="$JWT_SECRET" \
  BURROW_JWT_ISSUER="$JWT_ISSUER" \
  BURROW_JWT_AUDIENCE="$JWT_AUDIENCE" \
  BURROW_SERVER_ADDR="$SERVER_ADDR" \
  BURROW_BRIDGE_ADDR="$BRIDGE_BIND" \
  exec go run ./cmd/root server
) >/tmp/burrow-e2e-server.log 2>&1 &
SERVER_PID=$!

wait_for_http_ok "http://127.0.0.1:${SERVER_PORT}/healthz" 100 || {
  echo "[e2e] server health check failed" >&2; exit 1
}

# ---------------------------------------------------------------------------
# Single-client: data path + metrics
# ---------------------------------------------------------------------------

echo "[e2e] ── test: single client data path ──"

TOKEN_A="$(mint_bearer_token "$CLIENT_ID" "$JWT_AUDIENCE" "$JWT_ISSUER" "$JWT_SECRET" 300)"
CLIENT_PID="$(start_client "$CLIENT_ID" "$SERVER_URL" "$LOCAL_TARGET" "$TOKEN_A")"

echo "[e2e] waiting for bridge address for ${CLIENT_ID}"
BRIDGE_ADDR_A="$(wait_for_bridge_addr "$SERVER_PORT" "$CLIENT_ID" 100)" || {
  echo "[e2e] bridge address for ${CLIENT_ID} not available" >&2; exit 1
}
BRIDGE_PORT_A="${BRIDGE_ADDR_A##*:}"
echo "[e2e]   bridge A: ${BRIDGE_ADDR_A}"

wait_for_tcp 127.0.0.1 "$BRIDGE_PORT_A" 100 || {
  echo "[e2e] bridge listener A not reachable" >&2; exit 1
}

echo "[e2e] validating /metrics endpoint"
metrics="$(curl -fsS "$METRICS_URL")"
assert_contains "$metrics" "burrow_sessions_active"
assert_contains "$metrics" "burrow_streams_active"
assert_contains "$metrics" "burrow_stale_services_deleted_total"
assert_contains "$metrics" "burrow_stream_backpressure_drops_total"

echo "[e2e] validating tunnel data path for ${CLIENT_ID}"
if ! wait_for_tunnel_echo "smoke-ok" "$BRIDGE_ADDR_A" 60; then
  echo "[e2e] tunnel echo check failed for client A" >&2; exit 1
fi

# ---------------------------------------------------------------------------
# Two simultaneous clients: stream isolation
# ---------------------------------------------------------------------------

echo "[e2e] ── test: two simultaneous clients ──"

TOKEN_B="$(mint_bearer_token "$CLIENT_ID_B" "$JWT_AUDIENCE" "$JWT_ISSUER" "$JWT_SECRET" 300)"
CLIENT_B_PID="$(start_client "$CLIENT_ID_B" "$SERVER_URL" "$LOCAL_TARGET_B" "$TOKEN_B" "b")"

echo "[e2e] waiting for bridge address for ${CLIENT_ID_B}"
BRIDGE_ADDR_B="$(wait_for_bridge_addr "$SERVER_PORT" "$CLIENT_ID_B" 100)" || {
  echo "[e2e] bridge address for ${CLIENT_ID_B} not available" >&2; exit 1
}
BRIDGE_PORT_B="${BRIDGE_ADDR_B##*:}"
echo "[e2e]   bridge B: ${BRIDGE_ADDR_B}"

wait_for_tcp 127.0.0.1 "$BRIDGE_PORT_B" 100 || {
  echo "[e2e] bridge listener B not reachable" >&2; exit 1
}

echo "[e2e] verifying isolated echo: payload-a arrives only on bridge A"
if ! wait_for_tunnel_echo "payload-a" "$BRIDGE_ADDR_A" 40; then
  echo "[e2e] echo check failed for client A (isolation test)" >&2; exit 1
fi
if ! wait_for_tunnel_echo "payload-b" "$BRIDGE_ADDR_B" 40; then
  echo "[e2e] echo check failed for client B (isolation test)" >&2; exit 1
fi

echo "[e2e] verifying session metric shows 2 active sessions"
if ! wait_for_metric_equals "$METRICS_URL" "burrow_sessions_active" "2" 60; then
  got="$(metric_value "$METRICS_URL" "burrow_sessions_active" || true)"
  echo "[e2e] expected 2 active sessions, got ${got}" >&2; exit 1
fi

# ---------------------------------------------------------------------------
# Single client failure + reconnect
# ---------------------------------------------------------------------------

echo "[e2e] ── test: client failure and reconnect ──"

echo "[e2e] killing client A to simulate failure"
kill_tree "$CLIENT_PID"
wait "$CLIENT_PID" >/dev/null 2>&1 || true
CLIENT_PID=""

echo "[e2e] ensuring bridge A does not pass traffic while client A is down"
if ! wait_for_tunnel_down "should-fail" "$BRIDGE_ADDR_A" 40; then
  echo "[e2e] expected no echo on bridge A while client A is down" >&2; exit 1
fi

echo "[e2e] verifying client B is unaffected while client A is down"
if ! wait_for_tunnel_echo "b-unaffected" "$BRIDGE_ADDR_B" 40; then
  echo "[e2e] client B echo failed while client A was down" >&2; exit 1
fi

echo "[e2e] restarting client A to validate recovery"
TOKEN_A="$(mint_bearer_token "$CLIENT_ID" "$JWT_AUDIENCE" "$JWT_ISSUER" "$JWT_SECRET" 300)"
CLIENT_PID="$(start_client "$CLIENT_ID" "$SERVER_URL" "$LOCAL_TARGET" "$TOKEN_A" "reconnect")"

if ! wait_for_tunnel_echo "recover-ok" "$BRIDGE_ADDR_A" 80; then
  echo "[e2e] reconnect recovery check failed for client A" >&2; exit 1
fi

echo "[e2e] ── all checks passed ──"
