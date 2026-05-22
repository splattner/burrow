# Kubernetes WebSocket Reverse Tunnel

Initial scaffold for a single-binary Go project that supports:

- `server` mode: in-cluster endpoint for WebSocket sessions and stream routing.
- `client` mode: outbound connector exposing one local TCP target through the tunnel.

This repository currently contains an implementation skeleton with package boundaries wired for incremental development.

## Project Layout

- `cmd/root`: executable entrypoint and subcommand dispatch.
- `cmd/server`: server command wrapper.
- `cmd/client`: client command wrapper.
- `internal/config`: environment-based configuration.
- `internal/protocol`: frame types and control-frame codec.
- `internal/tunnel`: stream multiplexer skeleton.
- `internal/server`: server runtime skeleton.
- `internal/client`: client runtime skeleton.
- `internal/bridge`: TCP relay helpers.
- `internal/kube`: Kubernetes reconciliation stubs.
- `internal/metrics`: in-process counters.
- `internal/logging`: structured logging (logrus).
- `test`: placeholder for integration/e2e assets.

## Build

```bash
make build
```

Or directly:

```bash
go build ./...
```

## End-to-End Smoke And Failure Test

Run a local orchestration test that boots a local TCP echo target, server, and client, then validates:

- `/healthz` and `/metrics` availability
- tunnel data path end-to-end
- failure behavior when client is killed
- recovery behavior after client restart

```bash
make e2e-smoke
```

The script is located at `test/e2e/smoke.sh` and writes process logs to:

- `/tmp/krt-e2e-server.log`
- `/tmp/krt-e2e-client.log`
- `/tmp/krt-e2e-echo.log`

## Run

Server mode (flags):

```bash
go run ./cmd/root server --jwt-hmac-secret dev-secret --jwt-audience krt-server --jwt-issuer dev-local
```

Server mode (environment variables):

```bash
KRT_JWT_ALG=HS256 KRT_JWT_HMAC_SECRET=dev-secret KRT_JWT_AUDIENCE=krt-server KRT_JWT_ISSUER=dev-local go run ./cmd/root server
```

Client mode (flags):

```bash
go run ./cmd/root client --bearer-token "$JWT" --server-url ws://localhost:8080/ws --client-id client-a --local-target 127.0.0.1:5432
```

Client mode (environment variables):

```bash
KRT_BEARER_TOKEN=<signed-jwt> KRT_SERVER_URL=ws://localhost:8080/ws KRT_CLIENT_ID=client-a KRT_LOCAL_TARGET=127.0.0.1:5432 go run ./cmd/root client
```

## Authentication (JWT-Only)

Static shared-token authentication has been removed. The tunnel is JWT-only.

JWT verifier settings:

- `KRT_JWT_ALG` (default `RS256`)
- `KRT_JWT_PUBLIC_KEY_FILE` (recommended for asymmetric JWT validation)
- `KRT_JWT_HMAC_SECRET` (optional, dev/test)
- `KRT_JWKS_URL` (recommended for production key rotation)
- `KRT_JWKS_REFRESH` (JWKS refresh interval, default `5m`)
- `KRT_JWT_ISSUER` (optional expected issuer)
- `KRT_JWT_AUDIENCE` (optional expected audience)

When `KRT_JWKS_URL` is set, the server resolves JWT verification keys by `kid` from JWKS.

Identity binding rule:

- When a request is authenticated with JWT, the tunnel register `client_id` must equal the JWT `sub` claim.
- If they do not match, the server rejects registration and closes the session.

Client bearer configuration:

- `KRT_BEARER_TOKEN` (required for client websocket auth)
- `KRT_BEARER_TOKEN_FILE` (optional file-based token source, re-read on reconnect)

Token lifecycle and retry controls:

- `KRT_TOKEN_REFRESH_WINDOW` (default `30s`): proactive reconnect before `exp`.
- `KRT_CLIENT_RETRY_INTERVAL` (default `1s`): base backoff for transport failures.
- `KRT_CLIENT_AUTH_RETRY_INTERVAL` (default `5s`): base backoff for auth failures.

Auth failure signaling:

- Server returns explicit unauthorized codes for JWT timing errors (`token_expired`, `token_not_yet_valid`).
- Client applies auth-aware retry backoff to reduce reconnect storms.
- If using `KRT_BEARER_TOKEN_FILE`, the client will quickly retry and pick up rotated tokens.

Example JWT server rollout:

```bash
go run ./cmd/root server \
  --jwt-alg RS256 \
  --jwt-public-key-file /etc/krt/jwt-public.pem \
  --jwt-issuer https://issuer.example \
  --jwt-audience krt-server
```

Makefile-driven JWT run example:

```bash
make run-server JWT_ALG=HS256 JWT_HMAC_SECRET=jwt-secret JWT_AUDIENCE=krt-server
make run-client BEARER_TOKEN="<signed-jwt>" CLIENT_ID=client-a LOCAL_TARGET=127.0.0.1:5432
```

File-based rotation example:

```bash
make run-client BEARER_TOKEN_FILE=/var/run/krt/client.jwt CLIENT_ID=client-a LOCAL_TARGET=127.0.0.1:5432 TOKEN_REFRESH_WINDOW=45s
```

Makefile-driven JWKS example:

```bash
make run-server JWT_ALG=RS256 JWKS_URL=https://issuer.example/.well-known/jwks.json JWT_AUDIENCE=krt-server
make run-client BEARER_TOKEN="<signed-jwt-with-kid>" CLIENT_ID=client-a LOCAL_TARGET=127.0.0.1:5432
```

Quick dev bootstrap for a new JWT client (HS256):

1. Start server in JWT mode (shared dev secret):

```bash
make run-server-jwt-dev JWT_HMAC_SECRET=dev-secret JWT_AUDIENCE=krt-server JWT_ISSUER=dev-local
```

2. In another terminal, mint a short-lived token and start client in one command:

```bash
make run-client-jwt-dev CLIENT_ID=client-a LOCAL_TARGET=127.0.0.1:5432 SERVER_URL=ws://127.0.0.1:8080/ws JWT_HMAC_SECRET=dev-secret JWT_AUDIENCE=krt-server JWT_ISSUER=dev-local
```

This is equivalent to the generic server target:

```bash
make run-server JWT_ALG=HS256 JWT_HMAC_SECRET=dev-secret JWT_AUDIENCE=krt-server JWT_ISSUER=dev-local
```

Notes:

- The helper script generates JWT `sub` from `CLIENT_ID` automatically.
- JWT `sub` must match `CLIENT_ID` or registration is rejected.
- Token default TTL is 300s; override with `JWT_TTL_SECONDS` when running `scripts/run-client-jwt-dev.sh` directly.

## Kubernetes Manifests

Starter manifests are under `manifests/`:

- `serviceaccount.yaml`
- `role.yaml`
- `rolebinding.yaml`
- `deployment.yaml`
- `service.yaml`
- `ingress.yaml`

Adjust image, host, namespace, and ingress class as needed.
