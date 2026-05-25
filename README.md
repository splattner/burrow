<p align="center">
  <img src="logo.png" alt="Burrow logo" width="180">
</p>

# Burrow

Expose a TCP service running outside your cluster to workloads running inside it — without opening inbound firewall ports or configuring static routes.

The client runs wherever your service lives (laptop, edge node, private server). It dials outbound to a WebSocket endpoint on the server, which runs inside your cluster. Traffic from pods reaches your local service through that persistent tunnel connection. Because the client always initiates the connection, it works through NAT, firewalls, and most corporate networks.

![Burrow architecture overview](overview.png)

The server optionally manages Kubernetes `Service` objects automatically — one per connected client — so pods can reach tunnelled services by a stable DNS name.

---

## Contents

- [How it works](#how-it-works)
- [Quick start (dev)](#quick-start-dev)
- [Deploying to Kubernetes](#deploying-to-kubernetes)
- [Running the client](#running-the-client)
- [Expose command](#expose-command)
- [Configuration reference](#configuration-reference)
- [Authentication](#authentication)
- [Building from source](#building-from-source)

---

## How it works

1. The **server** runs in-cluster and listens on:
   - An HTTP/WebSocket port (default `:8080`) — clients connect here, pods call `/healthz` and `/metrics` here.
   - One TCP bridge port per connected client (random ephemeral port) — pods that want to reach the tunnelled service connect here.

2. The **client** runs outside the cluster. On startup it:
   - Dials the server's WebSocket endpoint with a signed JWT as its bearer token.
   - Registers its `client_id` and the `local-target` address it will forward traffic to.
   - Keeps the connection alive with heartbeats and reconnects automatically on failure.

3. When a pod connects to the bridge port, the server opens a new multiplexed stream over the active WebSocket session and the client forwards it to the local target.

4. When the client disconnects the server cleans up its associated Kubernetes `Service` (if Kube API mode is enabled).

---

## Quick start (dev)

Requires Go 1.25+ and Python 3 (used by the token-minting helper script).

**1. Start the server with a shared HS256 secret:**

```bash
make run-server-jwt-dev JWT_HMAC_SECRET=dev-secret JWT_AUDIENCE=burrow-server JWT_ISSUER=dev-local
```

**2. In a second terminal, mint a token and start the client:**

```bash
make run-client-jwt-dev \
  CLIENT_ID=client-a \
  LOCAL_TARGET=127.0.0.1:5432 \
  JWT_HMAC_SECRET=dev-secret \
  JWT_AUDIENCE=burrow-server \
  JWT_ISSUER=dev-local
```

The client connects to `ws://127.0.0.1:8080/ws` by default. Once registered, the server assigns a random bridge port. Traffic arriving at that port is forwarded to `127.0.0.1:5432` on the client machine.

**3. Test the tunnel:**

```bash
# Discover the bridge address assigned to your client, then connect
BRIDGE=$(curl -s http://127.0.0.1:8080/api/clients/client-a/bridge-addr)
nc $(echo $BRIDGE | cut -d: -f1) $(echo $BRIDGE | cut -d: -f2)
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
| `image` | Your built image (`ghcr.io/yourorg/burrow:tag`) |
| `BURROW_JWT_HMAC_SECRET` | Replace `change-me` with a real secret, or switch to `BURROW_JWT_PUBLIC_KEY_FILE` / `BURROW_JWKS_URL` |
| `BURROW_JWT_AUDIENCE` | Match the audience your tokens are issued for |
| `BURROW_JWT_ISSUER` | Match your token issuer |
| `BURROW_NAMESPACE` | Namespace where client `Service` objects are created |

Edit `manifests/ingress.yaml`:

- Set `spec.rules[0].host` to your actual domain.
- The ingress must support long-lived connections — the nginx annotations set `proxy-read-timeout` and `proxy-send-timeout` to `3600s`.

### Production JWT configuration

For production, use RS256 or ES256 with a JWKS endpoint instead of a shared secret:

```yaml
- name: BURROW_JWT_ALG
  value: "RS256"
- name: BURROW_JWKS_URL
  value: "https://your-idp.example/.well-known/jwks.json"
- name: BURROW_JWT_AUDIENCE
  value: "burrow-server"
- name: BURROW_JWT_ISSUER
  value: "https://your-idp.example"
```

Remove the `BURROW_JWT_HMAC_SECRET` entry when using JWKS.

---

## Running the client

The client binary runs wherever you want to expose a service from. Download a release binary or [build from source](#building-from-source).

### Minimal example

```bash
burrow client \
  --bearer-token "$JWT" \
  --server-url wss://burrow.example.com/ws \
  --client-id my-service \
  --local-target 127.0.0.1:5432
```

### Using a token file (recommended for long-running clients)

Token files are re-read on every reconnect, so token rotation requires no restart:

```bash
burrow client \
  --bearer-token-file /var/run/secrets/burrow/token.jwt \
  --server-url wss://burrow.example.com/ws \
  --client-id my-service \
  --local-target 127.0.0.1:5432
```

The client reconnects proactively before the token expires (controlled by `--token-refresh-window`).

### Makefile helpers

```bash
# Inline token
make run-client BEARER_TOKEN="$JWT" CLIENT_ID=my-service LOCAL_TARGET=127.0.0.1:5432

# Token file with custom refresh window
make run-client BEARER_TOKEN_FILE=/var/run/burrow/token.jwt CLIENT_ID=my-service LOCAL_TARGET=127.0.0.1:5432 TOKEN_REFRESH_WINDOW=45s

# Production server (JWKS)
make run-server JWKS_URL=https://idp.example/.well-known/jwks.json JWT_AUDIENCE=burrow-server
```

---

## Expose command

`burrow expose` is a one-shot command that:

1. Deploys the burrow server to Kubernetes (ServiceAccount, Role, RoleBinding, Deployment, Service, and optionally Ingress).
2. Generates an ephemeral HS256 key, stores it in a Kubernetes Secret, and mints a short-lived JWT for the client.
3. Starts the burrow client locally and connects it to the deployed server, forming the complete tunnel.
4. Cleans up all Kubernetes resources when the tunnel exits (unless `--keep` is set).

This is the fastest way to set up a tunnel without managing manifests manually.

### Modes

The tunnel mode (whether an Ingress is created) is controlled by `--hostname`. The Kubernetes Service type is controlled separately by `--service-type`.

| Mode | When | Server URL |
|---|---|---|
| **Ingress** | `--hostname` is set | `wss://<hostname>/ws` |
| **LoadBalancer** | `--hostname` not set | `ws://<lb-ip>:<server-port>/ws` |

`--service-type auto` (default) resolves to `ClusterIP` in Ingress mode and `LoadBalancer` otherwise. Override with an explicit type when needed — e.g. `--service-type NodePort` to use a node port even without an Ingress.

### Quick examples

```bash
# Expose a local PostgreSQL via Ingress (TLS, WebSocket-ready nginx annotations included)
burrow expose \
  --client-id pg \
  --local-target 127.0.0.1:5432 \
  --hostname tunnel.example.com

# Expose via LoadBalancer (no Ingress needed)
burrow expose \
  --client-id api \
  --local-target 127.0.0.1:8080

# Preview what Kubernetes resources would be created without deploying
burrow expose --client-id api --dry-run

# Keep server resources after the tunnel closes (reconnect later with --reuse)
burrow expose --client-id api --local-target 127.0.0.1:8080 --keep

# Reconnect to a previously kept deployment
burrow expose --client-id api --local-target 127.0.0.1:8080 --reuse

# Delete a kept deployment
burrow expose delete --client-id api
```

### Kubernetes resources created

| Resource | Purpose |
|---|---|
| `Secret burrow-<id>-auth` | Ephemeral HS256 key used to sign the client JWT |
| `ServiceAccount burrow-<id>` | Identity for the server Pod |
| `Role burrow-<id>` | Grants CRUD on `services` in the namespace |
| `RoleBinding burrow-<id>` | Binds the Role to the ServiceAccount |
| `Deployment burrow-<id>` | Runs the burrow server Pod |
| `Service burrow-<id>` | Type determined by `--service-type` (default: `ClusterIP` with Ingress, `LoadBalancer` otherwise) |
| `Ingress burrow-<id>` | Routes external HTTPS traffic to the server (Ingress mode only) |

All resources share the labels `app.kubernetes.io/managed-by=burrow` and `burrow.dev/server-name=<name>` (where `<name>` is `--server-name` if provided, otherwise `--client-id`).

### Customising with `--patch-*`

The `--patch-deployment`, `--patch-service`, and `--patch-ingress` flags accept a JSON [strategic merge patch](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/update-api-object-kubectl-patch/) applied to the respective resource before creation.

```bash
# Add resource requests and limits
burrow expose --client-id api --local-target 127.0.0.1:8080 \
  --patch-deployment '{"spec":{"template":{"spec":{"containers":[{"name":"server","resources":{"requests":{"cpu":"50m","memory":"64Mi"},"limits":{"cpu":"200m","memory":"128Mi"}}}]}}}}'

# Enforce a non-root, read-only container
burrow expose --client-id api --local-target 127.0.0.1:8080 \
  --patch-deployment '{"spec":{"template":{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65534},"containers":[{"name":"server","securityContext":{"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,"capabilities":{"drop":["ALL"]}}}]}}}}'
```

### Expose flags

| Flag | Default | Description |
|---|---|---|
| `--client-id` | — | Unique identifier for this session (required) |
| `--local-target` | — | Local `host:port` to forward tunnel traffic to (required unless `--dry-run`) |
| `--hostname` | — | Ingress hostname; when set, an Ingress resource is created |
| `--service-type` | `auto` | Kubernetes Service type: `auto`, `ClusterIP`, `NodePort`, `LoadBalancer`, `None`. `auto` uses `ClusterIP` with `--hostname`, `LoadBalancer` otherwise |
| `--connect-addr` | — | IP address (or `host:port`) to connect to at the TCP level instead of resolving `--hostname` via DNS. The hostname is still used for TLS SNI. Useful when DNS is not yet propagated after Ingress creation. |
| `--tls-secret` | — | TLS Secret name for Ingress; omit to use the controller's default cert |
| `--ingress-class` | auto-detect | IngressClass name |
| `--ingress-annotation` | — | Extra Ingress annotation in `key=value` format (repeatable) |
| `--image` | `ghcr.io/splattner/burrow:<version>` | Container image for the server |
| `--server-port` | `8080` | Port the server listens on inside the container |
| `--server-name` | `--client-id` | Kubernetes resource name prefix (e.g. `burrow-<server-name>`). Override to distinguish multiple deployments from the same `--client-id`. |
| `--namespace` | context default | Kubernetes namespace |
| `--kube-context` | current context | Kubernetes context |
| `--reuse` | false | Connect to an existing burrow deployment instead of creating one |
| `--keep` | false | Leave server resources in Kubernetes after the tunnel closes |
| `--wait-timeout` | `2m` | Maximum time to wait for the server to become available |
| `--dry-run` | false | Print Kubernetes resources without deploying |
| `--patch-deployment` | — | JSON strategic merge patch for the Deployment |
| `--patch-service` | — | JSON strategic merge patch for the Service |
| `--patch-ingress` | — | JSON strategic merge patch for the Ingress |

---

## Configuration reference

All flags can be set via environment variables with the `BURROW_` prefix. Flags take precedence over environment variables.

### Server

| Flag | Env var | Default | Description |
|---|---|---|---|
| `--jwt-alg` | `BURROW_JWT_ALG` | `RS256` | JWT signing algorithm |
| `--jwt-hmac-secret` | `BURROW_JWT_HMAC_SECRET` | — | HMAC secret for HS256/HS384/HS512 (dev/test) |
| `--jwt-public-key-file` | `BURROW_JWT_PUBLIC_KEY_FILE` | — | Path to PEM public key for RS256/ES256 |
| `--jwks-url` | `BURROW_JWKS_URL` | — | JWKS endpoint URL; keys resolved by `kid` |
| `--jwks-refresh` | `BURROW_JWKS_REFRESH` | `5m` | How often to refresh JWKS keys |
| `--jwt-issuer` | `BURROW_JWT_ISSUER` | — | Expected `iss` claim (optional) |
| `--jwt-audience` | `BURROW_JWT_AUDIENCE` | — | Expected `aud` claim (optional) |
| `--server-addr` | `BURROW_SERVER_ADDR` | `:8080` | WebSocket and HTTP listen address |
| `--tls-cert` | `BURROW_TLS_CERT` | — | Path to TLS certificate PEM file; enables server-side HTTPS/WSS when set together with `--tls-key` |
| `--tls-key` | `BURROW_TLS_KEY` | — | Path to TLS private key PEM file; enables server-side HTTPS/WSS when set together with `--tls-cert` |
| `--bridge-host` | `BURROW_BRIDGE_HOST` | — | Host to bind per-client bridge listeners on (e.g. `0.0.0.0` or `127.0.0.1`). Each client gets a random port. Empty disables bridging. |
| `--namespace` | `BURROW_NAMESPACE` | `default` | Namespace for auto-created client Services |
| `--enable-kube-api` | `BURROW_ENABLE_KUBE_API` | auto | Force Kubernetes Service reconciliation on (`true`) or off (`false`) |
| `--heartbeat-interval` | `BURROW_HEARTBEAT_INTERVAL` | `10s` | How often to send heartbeats |
| `--heartbeat-timeout` | `BURROW_HEARTBEAT_TIMEOUT` | `30s` | Disconnect client if no heartbeat within this window |
| `--sweep-interval` | `BURROW_SWEEP_INTERVAL` | `1m` | How often to check for stale disconnected Services |
| `--stale-service-age` | `BURROW_STALE_SERVICE_AGE` | `10m` | Delete a disconnected client's Service after this duration |
| `--log-level` | `BURROW_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |

### Client

| Flag | Env var | Default | Description |
|---|---|---|---|
| `--bearer-token` | `BURROW_BEARER_TOKEN` | — | JWT to send as the bearer token |
| `--bearer-token-file` | `BURROW_BEARER_TOKEN_FILE` | — | File path to read the JWT from (re-read on reconnect) |
| `--server-url` | `BURROW_SERVER_URL` | — | Server WebSocket URL, e.g. `wss://burrow.example.com/ws` |
| `--client-id` | `BURROW_CLIENT_ID` | — | Unique identifier for this client; must match JWT `sub` |
| `--local-target` | `BURROW_LOCAL_TARGET` | — | Local `host:port` to forward traffic to |
| `--token-refresh-window` | `BURROW_TOKEN_REFRESH_WINDOW` | `30s` | Reconnect this long before the token expires |
| `--client-retry-interval` | `BURROW_CLIENT_RETRY_INTERVAL` | `1s` | Base backoff interval for transport failures |
| `--client-auth-retry-interval` | `BURROW_CLIENT_AUTH_RETRY_INTERVAL` | `5s` | Base backoff interval for auth failures |
| `--tls-skip-verify` | `BURROW_TLS_SKIP_VERIFY` | `false` | Disable TLS certificate verification (self-signed or expired certs). Not safe for production. |
| `--log-level` | `BURROW_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |

---

## Authentication

The server accepts JWT bearer tokens only. The token is sent by the client in the WebSocket upgrade request as `Authorization: Bearer <token>`. Authentication is enforced **before** the WebSocket handshake completes — unauthenticated requests receive HTTP 401 without ever establishing a WebSocket connection.

### Transport security (TLS)

**Server-side TLS** — the server can terminate TLS directly by providing a certificate and private key:

```bash
burrow server \
  --tls-cert /etc/tls/tls.crt \
  --tls-key  /etc/tls/tls.key \
  --jwt-hmac-secret …
```

When both `--tls-cert` and `--tls-key` are set the server listens with HTTPS/WSS and the client must use `wss://`. The scheme in `WSURL()` switches automatically when using `burrow expose`. Both flags must be provided together — setting only one is a startup error.

This is most useful when running without an Ingress controller (e.g. bare LoadBalancer) and you still need transport encryption.

In other deployments:

- Use `wss://` (WebSocket Secure) for all client connections. The bearer token is in an HTTP header and is exposed in plaintext if the connection is unencrypted.
- Terminate TLS at an Ingress controller or load balancer. The `burrow expose --hostname …` command uses `wss://` automatically and adds nginx `proxy-read-timeout`/`proxy-send-timeout` annotations so long-lived tunnel connections are not dropped.
- LoadBalancer mode without server-side TLS defaults to `ws://` (plain text). Add TLS at the load balancer or enable `--tls-cert`/`--tls-key` before exposing to untrusted networks.

**Client — skip TLS verification**

For environments where the server certificate is self-signed or not yet trusted (e.g. during initial setup), the client can skip certificate verification:

```bash
burrow client \
  --server-url wss://burrow.example.com/ws \
  --tls-skip-verify \
  …
```

> **Warning:** `--tls-skip-verify` disables all certificate validation and makes the connection vulnerable to man-in-the-middle attacks. Use only in trusted, controlled environments.

### JWT claim validation

On every connection the server verifies:

| Check | Behaviour |
|---|---|
| Signature | Must be valid for the configured key and algorithm |
| `alg` header | Must exactly match `--jwt-alg` (default `RS256`) — `alg: none` and algorithm substitution are always rejected |
| `exp` | Token must not be expired (30-second clock-skew leeway applied) |
| `nbf` | If present, token must already be valid-for-use |
| `iss` | Checked only when `--jwt-issuer` is configured |
| `aud` | Checked only when `--jwt-audience` is configured |

Setting `--jwt-audience` and `--jwt-issuer` is **strongly recommended** in production: without them a JWT issued for another service is accepted by the burrow server.

### Identity binding

After the WebSocket handshake, the client sends a `register` frame containing its `client_id`. The server immediately checks that the JWT `sub` claim equals the `client_id`. A mismatch terminates the session with a `client identity mismatch` error. This prevents an authenticated client from claiming a different client's tunnel slot even if they hold a valid token.

### Key sources

Exactly one key source must be configured:

| Option | When to use | Security notes |
|---|---|---|
| `--jwt-hmac-secret` | Development and testing **only** | Shared symmetric secret — any party with the secret can forge tokens. Never use in production. |
| `--jwt-public-key-file` | Static asymmetric key | Server holds only the public key; the private key stays with the token issuer. Supports RS256/RS384/RS512, ES256/ES384/ES512, EdDSA. |
| `--jwks-url` | Production (OIDC-compatible issuers) | Keys resolved by `kid`, refreshed every `--jwks-refresh` (default `5m`). Supports key rotation without server restart. Cannot be combined with symmetric algorithms (HS*). |

`--jwks-url` is the recommended choice for production: it is asymmetric, OIDC-compatible, and allows key rotation without any server restart.

### Token rotation

When using `--bearer-token-file`, the client reads the file on every reconnect. The `--token-refresh-window` setting (default `30s`) causes the client to proactively reconnect before token expiry and pick up the refreshed token automatically, making rotation seamless.

### Auth error codes

When authentication fails, the server sends a typed error code. The client uses these codes to choose its retry backoff:

| Code | Cause | Client behaviour |
|---|---|---|
| `token_expired` | `exp` claim in the past | Retry with `--client-auth-retry-interval` backoff |
| `token_not_yet_valid` | `nbf` claim in the future | Retry with `--client-auth-retry-interval` backoff |
| `invalid_token` | Bad signature, wrong algorithm, or other JWT error | Retry with `--client-auth-retry-interval` backoff |
| `missing_bearer` | No `Authorization: Bearer` header present | Retry with `--client-auth-retry-interval` backoff |
| `verifier_config` | Server-side auth misconfiguration | Retry with `--client-auth-retry-interval` backoff |

Auth retries use `--client-auth-retry-interval` (default `5s`) as the base, which is intentionally longer than the transport failure interval (`--client-retry-interval`, default `1s`) to avoid hammering the server with invalid tokens.

### Bridge port security

Each connected client is assigned a dedicated TCP bridge port (random ephemeral port). Bridge connections carry no application-layer authentication — any TCP client that can reach the port will have its traffic forwarded to the client's `--local-target`.

Recommendations:

- Use a Kubernetes [NetworkPolicy](https://kubernetes.io/docs/concepts/services-networking/network-policies/) to restrict which pods can connect to bridge ports. Allow only the pods that legitimately need access to the tunnelled service.
- Set `--bridge-host` to a specific interface (e.g. `127.0.0.1` for loopback-only in dev). In cluster deployments `0.0.0.0` is typical. Leave it empty to disable bridging entirely.
- The Kubernetes `Service` auto-created per client (when `--enable-kube-api` is on) exposes the bridge port cluster-internally. Combine with NetworkPolicy to control which workloads can route to it.

### Kubernetes RBAC

The server Pod requires only CRUD on `services` in its own namespace. The `Role` created by `burrow expose` (and in `manifests/role.yaml`) is scoped to this minimum — no cluster-level permissions are needed.

Run the server as a non-root unprivileged user (`runAsNonRoot: true`, `runAsUser: 65534`). Bridge ports are ephemeral (Linux range 32768+), which requires no special Linux capability.

---

## Container images

Images are published to the GitHub Container Registry at `ghcr.io/splattner/burrow`.

| Tag | Description |
|---|---|
| `latest` | Most recent stable release |
| `v1.2.3` | Specific release version |
| `edge` | Latest commit on `main` (may be unstable) |
| `sha-abc1234` | Pinned to a specific commit |

```bash
# Pull the latest stable release
docker pull ghcr.io/splattner/burrow:latest

# Pull a specific version
docker pull ghcr.io/splattner/burrow:v1.2.3

# Pull the latest development build
docker pull ghcr.io/splattner/burrow:edge
```

Release images are signed with [Sigstore cosign](https://docs.sigstore.dev/cosign/overview/) using keyless signing. Verify a release image:

```bash
cosign verify ghcr.io/splattner/burrow:latest \
  --certificate-identity-regexp="https://github.com/splattner/burrow/" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com"
```

SBOM files (CycloneDX/SPDX) are attached to each [GitHub release](https://github.com/splattner/burrow/releases) as release assets.

---

## Building from source

Requires Go 1.25+.

```bash
# Build the binary
make build
# Output: bin/burrow

# Run tests
make test

# Run the local end-to-end smoke test
make e2e-smoke
```

The smoke test (`test/e2e/smoke.sh`) starts a local echo server, a server process, and a client process, then verifies the full data path, health endpoints, and reconnect behavior. Logs are written to `/tmp/burrow-e2e-*.log`.
