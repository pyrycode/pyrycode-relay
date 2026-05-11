# pyrycode-relay

Stateless WebSocket relay that routes traffic between mobile clients and a [`pyrycode`](https://github.com/pyrycode/pyrycode) binary running on a user's machine. Companion service to the pyry binary.

```
┌────────┐     WSS     ┌──────────┐     WSS     ┌────────────────┐
│ phone  │ ──────────> │  relay   │ <────────── │ pyrycode binary│
│ (N)    │             │(stateless)│             │ (1 per server) │
└────────┘             └──────────┘             └────────────────┘
```

The relay routes by an `x-pyrycode-server` header and never reads message payloads. The binary owns canonical state (conversations, sessions, message history); the relay holds zero per-user state.

## Wire protocol

Implements the [`v1` mobile protocol](https://github.com/pyrycode/pyrycode/blob/main/docs/protocol-mobile.md) defined in the pyrycode CLI repo. That document is the single source of truth — this binary is one of two server-side implementations (the pyry binary is the other).

## Status

**Pre-alpha.** Scaffold only; no routing logic yet. See open issues for current work.

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

The image is host-agnostic: it exposes `:80` and `:443` for autocert, and declares a volume mount point at `/var/lib/relay/autocert` for the cert cache. Host-specific deploy wiring (TLS termination policy, port publishing, volume backing, single-instance enforcement) lives in #38.

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
| `--insecure-listen` | (unset) | Listen address for plain HTTP (e.g. `:8080`). Disables autocert. Use only when fronted by a reverse proxy. |
| `--version` | | Print version and exit. |

## License

MIT — see [LICENSE](./LICENSE).
