# pyrycode-relay

Stateless WebSocket relay that routes traffic between mobile and desktop clients and a [`pyrycode`](https://github.com/pyrycode/pyrycode) binary running on a user's machine. Companion service to the pyry binary.

```
┌────────┐     WSS     ┌──────────┐     WSS     ┌────────────────┐
│ client │ ──────────> │  relay   │ <────────── │ pyrycode binary│
│ (N)    │             │(stateless)│             │ (1 per server) │
└────────┘             └──────────┘             └────────────────┘
```

The relay routes by an `x-pyrycode-server` header and never reads message payloads. The binary owns canonical state (conversations, sessions, message history); the relay holds zero per-user state.

## Wire protocol

Implements the [`v2` mobile protocol](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md) defined in the pyrycode CLI repo — the daemon's only wire since mid-2026. The `/v1/server` and `/v1/client` endpoint names are route paths, not protocol versions: the relay is content-blind, and the protocol version lives in each frame's `v` field. That document is the single source of truth — this binary is one of two server-side implementations (the pyry binary is the other).

## Status

**Production — LIVE.** Deployed to [Fly.io](https://fly.io) at `pyrycode-relay.pyryco.de`, live since 2026-05-29 (DENIC delegation landed, real Let's Encrypt cert, `/healthz` returns `200`). Full routing shipped — client ↔ binary frame forwarding, per-IP rate limiting, graceful shutdown, metrics, and autocert TLS termination. Deploys are operator-direct from a clean `main` (`flyctl deploy --remote-only`); see [`docs/deploy.md`](docs/deploy.md).

## Build

```bash
make build         # → bin/pyrycode-relay
make test          # go test ./...
make vet           # go vet
make lint          # gosec + govulncheck (requires both installed locally)
```

### Docker

```bash
docker build -t pyrycode-relay:dev .
docker run --rm pyrycode-relay:dev --version
```

The image is host-agnostic: it exposes `:80` and `:443` for autocert, and declares a volume mount point at `/var/lib/relay/autocert` for the cert cache. Host-specific deploy wiring (TLS termination policy, port publishing, volume backing, single-instance enforcement) lives in [`fly.toml`](./fly.toml) and is documented in [`docs/deploy.md`](docs/deploy.md).

## Run

Production (autocert):

```bash
sudo ./bin/pyrycode-relay --domain relay.example.com
```

The relay binds `:443` (WSS) and `:80` (ACME http-01 challenge). Both ports must be reachable from the public internet — Let's Encrypt issues the cert by hitting `:80` on first request to the domain. The first WSS request after a fresh start may take ~10–20s while the cert is issued and cached to `--cert-cache`. Subsequent restarts reuse the cached cert.

Behind a reverse proxy (TLS terminated upstream):

```bash
./bin/pyrycode-relay --insecure-listen :8080
```

Flags:

| Flag | Default | Notes |
|---|---|---|
| `--domain` | (required for autocert) | Public domain for Let's Encrypt cert issuance. Required when `--insecure-listen` is unset. |
| `--cert-cache` | `~/.pyrycode-relay/certs` | Directory for autocert's TLS certificate cache. Created with `0700` if missing; refuses to start if an existing dir is world- or group-readable. |
| `--https-listen` | `:443` | Bind address for the autocert TLS terminator (host:port). Pass `:8443` etc. when a substrate forwards external 443 to a high internal port. |
| `--http-listen` | `:80` | Bind address for the ACME HTTP-01 challenge listener (host:port). Pass `:8080` etc. for the same high-port substrate-forward pattern. |
| `--insecure-listen` | (unset) | Listen address for plain HTTP (e.g. `:8080`). Disables autocert. Use only when fronted by a reverse proxy. Mutually exclusive with `--http-listen` / `--https-listen`. |
| `--metrics-listen` | `127.0.0.1:9090` | Listen address for the `/metrics` endpoint. Must be a loopback IP literal (e.g. `127.0.0.1:9090`, `[::1]:9090`). Empty disables. |
| `--trust-x-forwarded-for` | `false` | Trust the `X-Forwarded-For` header as the source IP for per-IP rate limiting. Enable only behind a trusted reverse proxy — otherwise clients can spoof their source IP and bypass the rate limits. |
| `--version` | | Print version and exit. |

`:80` and `:443` are privileged. On a nonroot substrate that can't bind them (distroless/`:nonroot` containers running uid 65532, K8s restricted SCC, unprivileged systemd without `CAP_NET_BIND_SERVICE`), bind high internal ports via `--http-listen=:8080 --https-listen=:8443` and let the substrate forward external `:80`/`:443`. The production Fly deploy uses `--http-listen=:8080 --https-listen=:8443`. ACME HTTP-01 keeps working under any TCP-passthrough substrate. See [`docs/knowledge/features/autocert-tls.md`](docs/knowledge/features/autocert-tls.md) § Operational notes.

## License

MIT — see [LICENSE](./LICENSE).
