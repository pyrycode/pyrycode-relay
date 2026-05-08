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

## Run

```bash
./bin/pyrycode-relay --domain relay.example.com
```

Flags:

| Flag | Default | Notes |
|---|---|---|
| `--domain` | (required for autocert) | Public domain for Let's Encrypt cert issuance. Required when `--insecure-listen` is unset. |
| `--cert-cache` | `~/.pyrycode-relay/certs` | Directory for autocert's TLS certificate cache. |
| `--insecure-listen` | (unset) | Listen address for plain HTTP (e.g. `:8080`). Disables autocert. Use only when fronted by a reverse proxy. |
| `--version` | | Print version and exit. |

## License

MIT — see [LICENSE](./LICENSE).
