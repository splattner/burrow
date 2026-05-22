# Kubernetes WebSocket Reverse Tunnel

Expose a TCP service running outside your cluster to workloads running inside it — without opening inbound firewall ports or configuring static routes.

The client runs wherever your service lives (laptop, edge node, private server). It dials outbound to a WebSocket endpoint on the server, which runs inside your cluster. Traffic from pods reaches your local service through that persistent tunnel connection. Because the client always initiates the connection, it works through NAT, firewalls, and most corporate networks.

```
  ┌─────────────────────────────────────┐
  │           Kubernetes cluster         │
  │                                      │
  │  Pod ──► Service ──► krt-server     │
  │                           │          │
  └───────────────────────────┼──────────┘
                    WebSocket │ (outbound)
                              │
                         krt-client
                              │
                         Your service
                       (e.g. :5432)
```

The server optionally manages Kubernetes `Service` objects automatically — one per connected client — so pods can reach tunnelled services by a stable DNS name.

---

## Contents

- [How it works](#how-it-works)
- [Quick start (dev)](#quick-start-dev)
- [Deploying to Kubernetes](#deploying-to-kubernetes)
- [Running the client](#running-the-client)
- [Configuration reference](#configuration-reference)
- [Authentication](#authentication)
- [Building from source](#building-from-source)

---

## How it works

1. The **server** runs in-cluster and listens on two ports:
   - An HTTP/WebSocket port (default `:8080`) — clients connect here, pods call `/healthz` and `/metrics` here.
   - A TCP bridge port (default `:1111`) — pods that want to reach the tunnelled service connect here.

2. The **client** runs outside the cluster. On startup it:
   - Dials the server's WebSocket endpoint with a signed JWT as its bearer token.
   - Registers its `client_id` and the `local-target` address it will forward traffic to.
   - Keeps the connection alive with heartbeats and reconnects automatically on failure.

3. When a pod connects to the bridge port, the server opens a new multiplexed stream over the active WebSocket session and the client forwards it to the local target.

4. When the client disconnects the server cleans up its associated Kubernetes `Service` (if Kube API mode is enabled).

---

## Quick start (dev)

Requires Go 1.22+ and Python 3 (used by the token-minting helper script).

**1. Start the server with a shared HS256 secret:**

```bash
make run-server-jwt-dev JWT_HMAC_SECRET=dev-secret JWT_AUDIENCE=krt-server JWT_ISSUER=dev-local
```

**2. In a second terminal, mint a token and start the client:**

```bash
make run-client-jwt-dev \
  CLIENT_ID=client-a \
  LOCAL_TARGET=127.0.0.1:5432 \
  JWT_HMAC_SECRET=dev-secret \
  JWT_AUDIENCE=krt-server \
  JWT_ISSUER=dev-local
```

The client connects to `ws://127.0.0.1:8080/ws` by default. Traffic arriving at the server's bridge port (`:1111`) is forwarded to `127.0.0.1:5432` on the client machine.

**3. Test the tunnel:**

```bash
# Anything connecting to the bridge port reaches your local service
nc 127.0.0.1 1111
```

---

## Deploying to Kubernetes

Manifests are in `manifests/`. Apply them in order:

```bash
kubectl apply -f manifests/serviceaccount.yaml
kubectl apply -f manifests/role.yaml
kubectl apply -f manifests/rolebinding.yaml
kubectl apply -f manifests/deployment.yaml
kubectl apply -f manifests/service.yaml
kubectl apply -f manifests/ingress.yaml
```

Edit `manifests/deployment.yaml` before applying:

| Field | What to change |
|---|---|
| `image` | Your built image (`ghcr.io/yourorg/k8s-reverse-tunnel:tag`) |
| `KRT_JWT_HMAC_SECRET` | Replace `change-me` with a real secret, or switch to `KRT_JWT_PUBLIC_KEY_FILE` / `KRT_JWKS_URL` |
| `KRT_JWT_AUDIENCE` | Match the audience your tokens are issued for |
| `KRT_JWT_ISSUER` | Match your token issuer |
| `KRT_NAMESPACE` | Namespace where client `Service` objects are created |

Edit `manifests/ingress.yaml`:

- Set `spec.rules[0].host` to your actual domain.
- The ingress must support long-lived connections — the nginx annotations set `proxy-read-timeout` and `proxy-send-timeout` to `3600s`.

### Production JWT configuration

For production, use RS256 or ES256 with a JWKS endpoint instead of a shared secret:

```yaml
- name: KRT_JWT_ALG
  value: "RS256"
- name: KRT_JWKS_URL
  value: "https://your-idp.example/.well-known/jwks.json"
- name: KRT_JWT_AUDIENCE
  value: "krt-server"
- name: KRT_JWT_ISSUER
  value: "https://your-idp.example"
```

Remove the `KRT_JWT_HMAC_SECRET` entry when using JWKS.

---

## Running the client

The client binary runs wherever you want to expose a service from. Download a release binary or [build from source](#building-from-source).

### Minimal example

```bash
k8s-reverse-tunnel client \
  --bearer-token "$JWT" \
  --server-url wss://krt.example.com/ws \
  --client-id my-service \
  --local-target 127.0.0.1:5432
```

### Using a token file (recommended for long-running clients)

Token files are re-read on every reconnect, so token rotation requires no restart:

```bash
k8s-reverse-tunnel client \
  --bearer-token-file /var/run/secrets/krt/token.jwt \
  --server-url wss://krt.example.com/ws \
  --client-id my-service \
  --local-target 127.0.0.1:5432
```

The client reconnects proactively before the token expires (controlled by `--token-refresh-window`).

### Makefile helpers

```bash
# Inline token
make run-client BEARER_TOKEN="$JWT" CLIENT_ID=my-service LOCAL_TARGET=127.0.0.1:5432

# Token file with custom refresh window
make run-client BEARER_TOKEN_FILE=/var/run/krt/token.jwt CLIENT_ID=my-service LOCAL_TARGET=127.0.0.1:5432 TOKEN_REFRESH_WINDOW=45s

# Production server (JWKS)
make run-server JWKS_URL=https://idp.example/.well-known/jwks.json JWT_AUDIENCE=krt-server
```

---

## Configuration reference

All flags can be set via environment variables with the `KRT_` prefix. Flags take precedence over environment variables.

### Server

| Flag | Env var | Default | Description |
|---|---|---|---|
| `--jwt-alg` | `KRT_JWT_ALG` | `RS256` | JWT signing algorithm |
| `--jwt-hmac-secret` | `KRT_JWT_HMAC_SECRET` | — | HMAC secret for HS256/HS384/HS512 (dev/test) |
| `--jwt-public-key-file` | `KRT_JWT_PUBLIC_KEY_FILE` | — | Path to PEM public key for RS256/ES256 |
| `--jwks-url` | `KRT_JWKS_URL` | — | JWKS endpoint URL; keys resolved by `kid` |
| `--jwks-refresh` | `KRT_JWKS_REFRESH` | `5m` | How often to refresh JWKS keys |
| `--jwt-issuer` | `KRT_JWT_ISSUER` | — | Expected `iss` claim (optional) |
| `--jwt-audience` | `KRT_JWT_AUDIENCE` | — | Expected `aud` claim (optional) |
| `--server-addr` | `KRT_SERVER_ADDR` | `:8080` | WebSocket and HTTP listen address |
| `--bridge-addr` | `KRT_BRIDGE_ADDR` | — | TCP bridge listen address for pod traffic |
| `--namespace` | `KRT_NAMESPACE` | `default` | Namespace for auto-created client Services |
| `--enable-kube-api` | `KRT_ENABLE_KUBE_API` | auto | Force Kubernetes Service reconciliation on (`true`) or off (`false`) |
| `--heartbeat-interval` | `KRT_HEARTBEAT_INTERVAL` | `10s` | How often to send heartbeats |
| `--heartbeat-timeout` | `KRT_HEARTBEAT_TIMEOUT` | `30s` | Disconnect client if no heartbeat within this window |
| `--sweep-interval` | `KRT_SWEEP_INTERVAL` | `1m` | How often to check for stale disconnected Services |
| `--stale-service-age` | `KRT_STALE_SERVICE_AGE` | `10m` | Delete a disconnected client's Service after this duration |
| `--log-level` | `KRT_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |

### Client

| Flag | Env var | Default | Description |
|---|---|---|---|
| `--bearer-token` | `KRT_BEARER_TOKEN` | — | JWT to send as the bearer token |
| `--bearer-token-file` | `KRT_BEARER_TOKEN_FILE` | — | File path to read the JWT from (re-read on reconnect) |
| `--server-url` | `KRT_SERVER_URL` | — | Server WebSocket URL, e.g. `wss://krt.example.com/ws` |
| `--client-id` | `KRT_CLIENT_ID` | — | Unique identifier for this client; must match JWT `sub` |
| `--local-target` | `KRT_LOCAL_TARGET` | — | Local `host:port` to forward traffic to |
| `--token-refresh-window` | `KRT_TOKEN_REFRESH_WINDOW` | `30s` | Reconnect this long before the token expires |
| `--client-retry-interval` | `KRT_CLIENT_RETRY_INTERVAL` | `1s` | Base backoff interval for transport failures |
| `--client-auth-retry-interval` | `KRT_CLIENT_AUTH_RETRY_INTERVAL` | `5s` | Base backoff interval for auth failures |
| `--log-level` | `KRT_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |

---

## Authentication

The server accepts JWT bearer tokens only. The token is sent by the client in the WebSocket upgrade request as `Authorization: Bearer <token>`.

### Identity binding

The JWT `sub` claim must equal the `--client-id` the client registers with. If they differ, the server rejects the connection immediately.

### Supported key sources

Configure exactly one of:

| Option | When to use |
|---|---|
| `--jwt-hmac-secret` | Development and testing only |
| `--jwt-public-key-file` | Static asymmetric key (RS256, ES256) |
| `--jwks-url` | Production; keys rotated without server restart |

### Token rotation

When using `--bearer-token-file`, the client reads the file on every reconnect. Combined with `--token-refresh-window`, the client proactively reconnects before expiry and picks up the new token automatically, making rotation seamless.

---

## Building from source

Requires Go 1.22+.

```bash
# Build the binary
make build
# Output: bin/k8s-reverse-tunnel

# Run tests
make test

# Run the local end-to-end smoke test
make e2e-smoke
```

The smoke test (`test/e2e/smoke.sh`) starts a local echo server, a server process, and a client process, then verifies the full data path, health endpoints, and reconnect behavior. Logs are written to `/tmp/krt-e2e-*.log`.
