# PROXY protocol v2 on the HTTPS listener (`--https-proxy-protocol`)

Behind Fly's raw-TCP services, the socket peer the relay sees is Fly's edge
proxy, not the client — so [the per-IP rate limiter](rate-limit-middleware.md)
put every client in one bucket, and a post-deploy reconnect storm throttled
every legitimate daemon and phone together. This feature makes the autocert
HTTPS listener read a [PROXY protocol v2](https://www.haproxy.org/download/2.0/doc/proxy-protocol.txt)
header — the client's real address, prepended by Fly's edge — before the TLS
handshake, so `RemoteAddr`, and therefore `ClientIP` and the rate limiter, see
the real client IP. Off by default; opt in with `--https-proxy-protocol`.
Neither `ClientIP` (`internal/relay/client_ip.go`) nor the rate-limit
middleware changed — they already trusted `r.RemoteAddr`. (#110)

## What it does

`internal/relay/proxyproto.go`:

```go
func NewProxyProtoListener(inner net.Listener, headerTimeout time.Duration) (net.Listener, error)
```

Wraps a `net.Listener` (built with `github.com/pires/go-proxyproto v0.15.0`)
so every accepted connection must open with a PROXY v2 header, read within
`headerTimeout`, before any other byte. The header's source address becomes
the connection's `RemoteAddr`.

Two deliberate narrowings beyond the library's defaults, both closing paths
where the library would otherwise leave `RemoteAddr` on the socket peer
while reads still succeed:

- **`ConnPolicy` is set explicitly to `proxyproto.REQUIRE`**, not left to
  `proxyproto.DefaultPolicy` — a mutable package-level global.
- **`ValidateHeader: validateProxyHeader`** accepts only `Version == 2`,
  `Command.IsProxy()` (rejects `LOCAL`), and a `*net.TCPAddr` source
  (rejects `UNSPEC` / unix transports). A `LOCAL`-command header or a v1
  `UNKNOWN` header is a *syntactically valid* header the library would
  otherwise honour by silently falling back to the socket peer — exactly the
  address this feature exists to stop trusting.

`headerTimeout <= 0` is a constructor error — the library reads `0` as its
own 10s default and a negative value as "no timeout", neither of which the
caller asked for.

### Failure path

A connection whose header is missing, malformed, rejected by
`validateProxyHeader`, or later than `headerTimeout` fails its first `Read`.
That `Read` is the TLS handshake, called inside `http.Server`'s per-connection
goroutine — so `http.Server` closes the connection with **zero bytes
written** and no handler ever runs. There is no fallback to the socket peer
address at any point in this path. Because the header read happens lazily in
the connection's own goroutine (not inside `Accept`), a stalled header holds
one goroutine and one file descriptor for at most `headerTimeout` and never
blocks other connections from being accepted.

## Wiring (`cmd/pyrycode-relay/main.go`)

- **`--https-proxy-protocol`** (bool, default `false`). Usage string names
  the Fly `proxy_proto` pairing and states that every connection without a
  valid header is refused.
- **`proxyHeaderTimeout = 5 * time.Second`** — a `const` at the top of
  `main.go`, matching the listeners' existing `ReadHeaderTimeout` (see
  `docs/threat-model.md` § *DoS resistance*). Fly's edge writes the header
  immediately on connect, so a legitimate header never approaches it.
- **Mutual exclusion with `--insecure-listen`.** `--https-proxy-protocol`
  joined the existing `fs.Visit`-based guard from
  [#96](autocert-tls.md) that already rejects `--http-listen` /
  `--https-listen` alongside `--insecure-listen` — exit code 2. The new flag
  configures the autocert HTTPS listener specifically and would silently
  no-op in insecure mode.
- **The `listen` closure for `httpsSrv`**: flag off keeps
  `s.ListenAndServeTLS("", "")` verbatim (byte-identical to pre-#110
  behaviour). Flag on: `net.Listen("tcp", s.Addr)` →
  `relay.NewProxyProtoListener(ln, proxyHeaderTimeout)` → `s.ServeTLS(pln,
  "", "")`. The wrapper has to sit under `ServeTLS`'s `tls.NewListener`,
  not on top of it, because the PROXY header precedes the TLS `ClientHello`.
  `CheckListenerPorts` ([listener port allowlist](listener-port-allowlist.md))
  is unaffected — the bound port doesn't change, only what wraps the raw
  listener.
- The `starting` log line gained `https_proxy_protocol` (bool). `main` is
  outside `internal/relay`'s log-key allowlist glob (`TestLogKeysAreAllowlisted`
  only walks `internal/relay/*.go`), and a boolean config echo carries no
  attacker-influenced content.

## `fly.toml`

The external `443` port gained the one handler this manifest allows:

```toml
[[services.ports]]
  port = 443
  handlers = ["proxy_proto"]
  proxy_proto_options = { version = "v2" }
```

`proxy_proto` prepends a header and otherwise passes the TCP stream through
unchanged — TLS still terminates in the relay via autocert, unlike `["tls"]`
or `["http"]`, either of which would terminate at Fly's edge (see
[Fly.io deploy § TCP passthrough](fly-deploy.md#tcp-passthrough-not-flys-http-proxy)).
`[processes] app` gained `--https-proxy-protocol`. Port `80` and the ACME
HTTP-01 listener are untouched — no rate limiter sits on that path, and the
ACME challenge exchange doesn't need the client's address.

**The `proxy_proto` handler and `--https-proxy-protocol` must deploy and
roll back together.** Handler on / flag off: every connection's PROXY header
reaches the TLS handshake as unexpected bytes, and the handshake fails.
Flag on / handler off: every connection lacks a header, and the listener
refuses it. Either mismatch fails 443 **closed**, not open — see
[`docs/deploy.md` § *fly.toml gotchas*](../../deploy.md#flytoml-gotchas) for
the operator-facing version of this coupling.

## What it deliberately does not do

- **No trust-anchoring on the upstream connection.** Under `REQUIRE`, the
  header is honoured from *any* TCP peer that can reach the listener — the
  wrapper narrows the header's *shape*, not *who sent it*. On Fly, anything
  on the app's private 6PN network that can reach internal port 8443
  directly (bypassing Fly's public edge) can forge a source address and
  pick its own rate-limit bucket. That is an org-internal attacker, not an
  internet one; restricting upstreams to Fly's proxy IP ranges would need
  ranges Fly doesn't publish as stable. Tracked as a deferred item in
  [`docs/security-followups.md`](../../security-followups.md).
- **No change to `ClientIP` or the rate-limit middleware.** Both already
  read `r.RemoteAddr` (`internal/relay/client_ip.go`,
  [rate-limit middleware](rate-limit-middleware.md)); this feature only
  changes what that value *is* on the HTTPS listener.
- **No change to the HTTP-01 listener on `:80`.** No rate limiter, no
  `RemoteAddr`-derived logic on that path.

## Testing

`internal/relay/proxyproto_test.go` (package `relay`), driving a real TLS
handshake via `httptest.NewUnstartedServer` with the `Listener` swapped for
the wrapper and `StartTLS`, plus a client `DialContext` that writes a v2
header built with `proxyproto.Header.WriteTo`:

- `TestValidateProxyHeader` — table: v2 PROXY TCPv4/TCPv6 accept; v1,
  `LOCAL`, `UNSPEC`, unix-stream all reject with `ErrProxyHeaderRejected`.
- `TestNewProxyProtoListener_RejectsNonPositiveTimeout`.
- `TestProxyProtoListener_SeparateBucketsPerSource` — two sources with
  different header addresses land in separate rate-limit buckets; exhausting
  one source's burst does not 429 the other (AC1).
- `TestProxyProtoListener_RejectsBadHeaderUnserved` — missing header, a
  malformed v2 signature, and a `LOCAL` header all fail the request with
  zero handler invocations and no socket-peer fallback (AC2).
- `TestProxyProtoListener_StalledHeader` — a connection that writes nothing
  doesn't block a concurrent, well-formed request; the stalled connection is
  itself closed within the bounded timeout (AC2).

`cmd/pyrycode-relay/main_e2e_test.go`:

- `TestRun_InsecureMutexWithAutocertListenFlags` gained the
  `insecure+https-proxy-protocol` row — exit 2 (AC3).
- `TestRun_HTTPSProxyProtocolWiring` boots `run` in autocert mode with and
  without the flag and sends a plain (headerless) request to the HTTPS
  port: flag off reproduces Go's stock "HTTP request to an HTTPS server"
  `400` (unchanged behaviour, AC3); flag on closes the connection with zero
  bytes. No ACME traffic is triggered either way.

## Related

- [Codebase: #110](../codebase/110.md) — implementation notes for this
  ticket.
- [Per-IP rate-limit middleware](rate-limit-middleware.md) § *Trust model*
  — the consumer of the address this feature now supplies in production.
- [Fly.io deploy](fly-deploy.md) § *TCP passthrough, not Fly's HTTP proxy*
  — the substrate decision this feature corrects the client-IP claim for.
- [ADR-0012](../decisions/0012-go-proxyproto-for-proxy-header-parsing.md) —
  why a maintained parser library, not a hand-rolled one.
- [Threat model](../../threat-model.md) § *DoS resistance*, § *Supply chain*.
- [`docs/security-followups.md`](../../security-followups.md) — the
  org-internal source-spoofing residual risk.
