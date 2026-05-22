# Agent Context

This document is for coding agents and AI assistants. It describes the codebase, conventions, and current state so you can work effectively without re-discovering everything from scratch.

## What this project is

A single Go binary (`burrow`) with two runtime modes — `server` and `client` — that together create a reverse WebSocket tunnel. The client runs outside a Kubernetes cluster and dials outbound to the server running inside it. Traffic from pods reaches an outside service through that persistent connection.

```
Pod → bridge TCP port → server → WebSocket stream → client → local TCP service
```

The server optionally auto-creates and cleans up Kubernetes `Service` objects for connected clients.

## Module and language

- Module: `github.com/splattner/burrow`
- Go 1.22
- All code is under `cmd/` (entrypoints) and `internal/` (library packages)

## Key dependencies

| Package | Role |
|---|---|
| `github.com/spf13/cobra` + `viper` | CLI subcommands; flag > env > default precedence |
| `github.com/gorilla/websocket` | WebSocket transport |
| `github.com/golang-jwt/jwt/v5` | JWT verification (HMAC, RSA, EC, EdDSA, JWKS) |
| `k8s.io/client-go` | Kubernetes Service reconciler |
| `github.com/sirupsen/logrus` | Structured leveled logging |

## Directory layout

```
cmd/
  root/       Cobra root command; registers server and client subcommands; binds --log-level
  server/     server subcommand: validates config, builds logger, calls internal/server
  client/     client subcommand: validates config, builds logger, calls internal/client
internal/
  config/     Single Config struct; LoadFromViper; ValidateServer; ValidateClient
  auth/       JWT Verifier; typed ErrorCode values for auth rejection reasons
  protocol/   Binary frame codec (14-byte header + payload); control and data frame types
  tunnel/     Stream Multiplexer; HeartbeatTracker
  server/     Server runtime: WebSocket handler, stream router, bridge listener, stale sweep
  client/     Client runtime: outbound dialer, register loop, local TCP forwarder
  bridge/     TCP relay helpers (bidirectional copy)
  kube/       Kubernetes Service reconciler (create/update/delete per client)
  metrics/    In-process counters (sessions, streams, drops, stale deletes); /metrics endpoint
  logging/    logrus factory: New(level), NoOp() for tests
manifests/    Kubernetes deployment manifests
scripts/      run-client-jwt-dev.sh: mints HS256 JWT and starts client
test/e2e/     smoke.sh: boots echo + server + client locally, asserts full data path
```

## Configuration model

**One shared `Config` struct** covers both modes. Fields not relevant to a mode are ignored at runtime.

`LoadFromViper(v)` populates it. `ValidateServer(cfg)` / `ValidateClient(cfg)` enforce mode-specific required fields — these are called in the subcommand `RunE`, not in `LoadFromViper`.

**Viper binding:**
- Env prefix: `BURROW_`
- `AutomaticEnv()` is on
- Key separator in env vars: `-` → `_` (e.g. `jwt-hmac-secret` → `BURROW_JWT_HMAC_SECRET`)
- All flags are bound via `v.BindPFlag(key, flags.Lookup(key))` in the subcommand constructor
- Precedence: CLI flag > env var > default

`LoadFromViper` is called from both `cmd/server` and `cmd/client` — it does not validate; call the appropriate `Validate*` function after it.

## Authentication

- **JWT-only** (static bearer tokens were removed in a prior refactor)
- The server validates the token in the WebSocket upgrade request (`Authorization: Bearer <token>`)
- Configure exactly one key source: `--jwt-hmac-secret` (dev), `--jwt-public-key-file` (static asymmetric), or `--jwks-url` (production key rotation)
- **Identity binding:** the JWT `sub` claim must equal the client's `--client-id`. Mismatch → server closes the session immediately after register
- `auth.ErrorCode` values: `token_expired`, `token_not_yet_valid`, `invalid_token`, `missing_bearer`, `verifier_config` — the server sends these in error frames; the client uses them to choose retry backoff

## Protocol

Frames are binary WebSocket messages with a 14-byte header:

```
[version:1][kind:1][stream_id:8][payload_len:4][payload:N]
```

- `kind=1` (KindControl): payload is JSON-encoded `ControlFrame`
- `kind=2` (KindData): raw TCP bytes for stream `stream_id`

Control frame types (see `internal/protocol`):
`register`, `register_ack`, `heartbeat`, `heartbeat_ack`, `open`, `open_ack`, `close`, `error`

## Server internals

- One active WebSocket session at a time (single-tenant v1)
- Incoming pod connections on the bridge port → server calls `mux.OpenStream(id, target)` → sends `open` frame to client
- Client responds with `open_ack` → bridge starts relaying bytes
- Data frames shuttle raw bytes bidirectionally over the single WebSocket connection
- `HeartbeatTracker` tracks liveness; no heartbeat within timeout → session closed
- `kube.Reconciler` creates/updates/deletes a `Service` per client on connect/disconnect
- Stale-sweep goroutine runs on `SweepInterval`; deletes Services for clients disconnected longer than `StaleServiceAge`
- `/healthz` and `/metrics` are served on the same port as `/ws`

## Client internals

- Dials `--server-url` with exponential backoff (base: `RetryInterval`, auth failures: `AuthRetryInterval`)
- Sends `register` frame with `client_id` + `local-target`; waits for `register_ack`
- Token file (`--bearer-token-file`) is re-read on every reconnect
- Proactive reconnect `TokenRefreshWindow` before JWT expiry
- Each `open` frame from server → client opens a local TCP connection to `LocalTarget` and relays bytes

## Logging

Use `*logrus.Logger` directly — **no wrapper type**. Obtain the logger via `logging.New(cfg.LogLevel)` in the subcommand `RunE`, then pass it into `server.New(cfg, logger)` / `client.New(cfg, logger)`. Tests use `logging.NoOp()`.

Log level conventions used in the codebase:
- `Errorf` — I/O failures, decode errors, bridge errors
- `Warnf` — rejected registrations, heartbeat timeouts, backpressure drops
- `Infof` — startup, stale sweep summaries
- `Debugf` — token refresh skips

## Testing

- `go test ./...` runs all unit and integration tests
- Integration tests live in `internal/server/server_client_integration_test.go` (`package server_test`) — they spin up a real server and client over loopback
- Unit tests within a package use `package server` / `package client` (white-box access)
- All constructors that accept `*logrus.Logger` must be called with `logging.NoOp()` in tests

**Do not** call `ValidateServer` / `ValidateClient` in tests — pass a `Config` literal directly to `server.New` / `client.New`.

## Build and run commands

```bash
make build        # produces bin/burrow
make test         # go test ./...
make e2e-smoke    # local end-to-end smoke test

make run-server-jwt-dev JWT_HMAC_SECRET=dev-secret JWT_AUDIENCE=burrow-server JWT_ISSUER=dev-local
make run-client-jwt-dev CLIENT_ID=client-a LOCAL_TARGET=127.0.0.1:5432

make run-server JWKS_URL=https://idp.example/.well-known/jwks.json JWT_AUDIENCE=burrow-server
make run-client BEARER_TOKEN="$JWT" CLIENT_ID=client-a LOCAL_TARGET=127.0.0.1:5432
```

Makefile variables mirror flag names without the `BURROW_` prefix (e.g. `JWT_HMAC_SECRET`, `BEARER_TOKEN`, `CLIENT_ID`).

## Current state and known scope

**Implemented and tested:**
- Full WebSocket tunnel (register, heartbeat, stream open/close, data relay)
- JWT authentication (HMAC, RSA, EC, EdDSA, JWKS with refresh)
- Identity binding (`sub` == `client_id`)
- Auth-aware retry backoff on the client
- TCP bridge listener (pod → server → client → local service)
- Kubernetes Service reconciler with stale sweep
- Mode-specific config validation
- Structured logging (logrus throughout)
- Metrics endpoint
- Integration tests for relay, bridge, reconnect, auth rejection

**Intentionally excluded from v1:**
- Multi-tenant isolation (only one active client session at a time)
- Multiple services per client
- HA / session state replication
- OIDC, mTLS
- HTTP-aware routing

## Conventions to follow

- **Do not add a custom logger wrapper** — use `*logrus.Logger` directly
- **Do not call `LoadFromEnv` in tests** — construct `config.Config{}` literals
- **Validation belongs in the subcommand `RunE`**, not in `LoadFromViper`
- **Flag names use kebab-case**; env vars use `BURROW_` + SCREAMING_SNAKE_CASE
- **All new packages under `internal/`** — nothing exported at the module root
- Error messages that reference configuration options must mention both the flag (`--flag-name`) and the env var (`BURROW_FLAG_NAME`)
