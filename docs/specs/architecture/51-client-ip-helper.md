# Spec: client-IP extraction helper (#51)

## Files to read first

- `internal/relay/server_endpoint.go:129-136` — existing unexported `remoteHost` helper. The new exported helper supersedes its shape but does NOT replace it in this ticket (no callers move; that is the wiring ticket's job).
- `internal/relay/healthz.go:1-57` — package convention for an exported `http`-touching primitive: short `package relay` doc comment, single function, no init state.
- `internal/relay/registry.go` / `internal/relay/registry_test.go` — confirms tests live in `package relay` (not `relay_test`) so they can reach unexported helpers and constants. Mirror this for the new test file.
- `internal/relay/server_endpoint_test.go:1-16` — established import style (`net/http`, `net/http/httptest`, standard-library-first ordering). The new test file follows the same shape.
- `docs/threat-model.md` § *DoS resistance — connection floods, slow-loris, fork-bomb retry* (l.32-40) — the "Future hardening: per-IP rate limit" sentence this helper feeds.
- `docs/PROJECT-MEMORY.md` § *Patterns established* — loud-failure pattern (relevant context for why the empty-string fallback is "deny by caller," not silent-passthrough).

## Context

The relay's threat model (§ DoS resistance) flags per-IP throttling as deferred future hardening; sibling ticket #50 (per-IP rate-limit type) needs a stable client-IP string per request. This ticket ships **only** the extraction primitive so its trust-model semantics — when to read `X-Forwarded-For` (XFF), when to ignore it — can be reviewed and tested in isolation, before any caller wires it in.

The pre-existing `remoteHost` in `server_endpoint.go:129-136` is a **logging** helper: it strips the port from `r.RemoteAddr` and falls back to `r.RemoteAddr` verbatim on parse error. It is unexported, has no XFF handling, and (importantly) returns a non-empty string in all cases — appropriate for log lines, wrong for a rate-limit key. The new helper is a **security** primitive: it must distinguish "I have a usable source IP" from "I do not," and it must encode the trust decision at the call site (the caller opts in to trusting XFF).

The two helpers diverge on contract, not on mechanics, which is why we add a new export rather than mutate `remoteHost`. `remoteHost` keeps its logging job; the wiring ticket (#50 or its sibling) will decide whether logging migrates to the new helper, and that is out of scope here.

## Design

### Single exported function

`internal/relay/client_ip.go`:

```go
package relay

import (
    "net"
    "net/http"
    "strings"
)

// ClientIP extracts the client's source IP from an incoming HTTP request.
//
// When trustForwardedFor is false, ClientIP returns the host portion of
// r.RemoteAddr with the port stripped. Use this in deployments where the
// relay is directly internet-facing: X-Forwarded-For is attacker-controlled
// and must be ignored.
//
// When trustForwardedFor is true, ClientIP returns the left-most entry of
// X-Forwarded-For (the original client per the de-facto convention) with
// surrounding whitespace stripped. If the header is absent or yields no
// non-empty entry, ClientIP falls back to the host portion of r.RemoteAddr.
// Use this only when the relay is fronted by a reverse proxy the operator
// trusts to set XFF correctly.
//
// ClientIP returns the empty string only when no usable source is available
// (RemoteAddr cannot be parsed and either XFF was not consulted or yielded
// nothing). Callers MUST treat the empty string as "deny": the rate-limit
// wiring ticket enforces this. ClientIP itself performs no policy.
//
// The returned string is the raw host portion as it appears on the wire —
// no canonicalisation (no IPv6 lower-casing, no zone-id stripping). Callers
// that need a canonical form must apply net.ParseIP themselves.
func ClientIP(r *http.Request, trustForwardedFor bool) string {
    if trustForwardedFor {
        if v := r.Header.Get("X-Forwarded-For"); v != "" {
            // Left-most entry = original client per de-facto convention.
            if comma := strings.IndexByte(v, ','); comma >= 0 {
                v = v[:comma]
            }
            if first := strings.TrimSpace(v); first != "" {
                return first
            }
        }
    }
    host, _, err := net.SplitHostPort(r.RemoteAddr)
    if err != nil {
        return ""
    }
    return host
}
```

Key shape decisions:

- **One function, no struct, no constructor.** The helper is pure — no I/O, no goroutines, no state. The signature already encodes the only policy bit (trust). Wrapping it in a type would be ceremony.
- **`trustForwardedFor` is a bool, not a more elaborate "trusted-proxy-CIDR list."** The AC names a single bool; the threat model's near-term need is direct-vs-fronted (a binary), and a CIDR allowlist is an obvious follow-up the wiring ticket can take. Naming the parameter `trustForwardedFor` (not `trustProxies`) keeps the scope precise.
- **Left-most XFF entry, no header chain walk.** AC explicitly names left-most. The other camp (right-most-trusted) requires an allowlist of trusted hops the relay does not have. With the bool-only contract, left-most is the only well-defined choice; the wiring ticket can introduce a hop-aware variant if/when CIDR trust lists exist.
- **`r.Header.Get` not `r.Header.Values`.** XFF is canonically a single comma-separated header. `Header.Get` returns the first occurrence's full value; if a malicious client sends `X-Forwarded-For: a` plus a second `X-Forwarded-For: b` header, `Header.Get` returns `a`. Either way the helper takes the left-most token of the first header. This matches Go's `net/http.Request.Header` semantics; the test asserts the single-header case explicitly. The multi-header case is **out of scope** for this ticket and called out under § *Open questions*.
- **`strings.IndexByte` over `strings.SplitN`.** Avoids the slice allocation when the header has multiple entries — a non-zero benefit at rate-limit-decision frequency. Same effect.
- **`net.SplitHostPort` for the `RemoteAddr` parse.** Handles `host:port`, `[ipv6]:port`, and surfaces an error on malformed input — exactly the discriminator we need to return `""`. The existing `remoteHost` falls back to `r.RemoteAddr` on parse failure; we deliberately diverge (empty-string → "deny") because the consumer is a rate-limit gate, not a log line.
- **`RemoteAddr` is **always** consulted as the fallback even when `trustForwardedFor` is true.** This matters when the proxy strips XFF or the request bypasses the proxy somehow. The AC names this explicitly.

### Data flow

```
*http.Request ──► ClientIP(r, trust=false) ──► SplitHostPort(r.RemoteAddr)
                                                    │
                                          ok ───────┴─────► host
                                          err ──► "" (caller denies)

*http.Request ──► ClientIP(r, trust=true)  ──► r.Header.Get("X-Forwarded-For")
                                                    │
                                       non-empty ───┴─► IndexByte(',') ──► TrimSpace ──► first
                                                    │                          │
                                                    │                       empty ──► fallback
                                                empty ──────────────────────────► fallback
                                                                                       │
                                                                                       ▼
                                                                          SplitHostPort(r.RemoteAddr)
                                                                                       │
                                                                            ok ──► host
                                                                            err ─► ""
```

### Concurrency model

None. The helper reads two fields of `*http.Request` (`RemoteAddr` and `Header`) and returns a string. Both fields are stable for the request lifetime; the net/http server does not mutate them concurrently with handler code. No locks, no goroutines, no shared state.

### Error handling

The helper has one error path: `net.SplitHostPort(r.RemoteAddr)` failed and no XFF entry was used. The chosen surface is **empty-string return**, not a `(string, error)` pair, because:

- Every caller would handle the error identically (treat as deny).
- The AC names the empty-string fallback explicitly.
- A `(string, error)` pair would push a meaningless `if err != nil` check into every caller — the kind of ceremony [`CODING-STYLE.md`] flags as anti-idiomatic. The helper's docstring promises the deny semantics; the wiring ticket enforces them in code.

The helper does NOT log on the parse-failure path. Logging is the caller's responsibility; this is a pure function. The wiring ticket may choose to emit a debug log on `""` returns if observed in production, but that is policy, not primitive.

### Testing strategy

A new test file `internal/relay/client_ip_test.go` in `package relay` (matches the rest of the package per convention). A single table-driven test exercises the matrix the AC enumerates. The test uses `httptest.NewRequest` to construct requests with controlled `RemoteAddr` and headers — no real listener, no goroutines.

Required test cases (one row each):

| Name | `RemoteAddr` | XFF header | `trustForwardedFor` | Want |
|---|---|---|---|---|
| `RemoteAddr_IPv4_WithPort` | `192.0.2.5:54321` | (none) | `false` | `192.0.2.5` |
| `RemoteAddr_IPv6_Bracketed` | `[2001:db8::1]:54321` | (none) | `false` | `2001:db8::1` |
| `RemoteAddr_Loopback_IPv6` | `[::1]:8080` | (none) | `false` | `::1` |
| `RemoteAddr_MalformedNoPort` | `192.0.2.5` | (none) | `false` | `""` |
| `RemoteAddr_Empty` | `` (empty) | (none) | `false` | `""` |
| `XFF_Disabled_HeaderIgnored` | `192.0.2.5:54321` | `203.0.113.7` | `false` | `192.0.2.5` |
| `XFF_Enabled_Single` | `10.0.0.1:443` | `203.0.113.7` | `true` | `203.0.113.7` |
| `XFF_Enabled_MultiEntry` | `10.0.0.1:443` | `203.0.113.7, 192.0.2.10, 198.51.100.4` | `true` | `203.0.113.7` |
| `XFF_Enabled_LeadingWhitespace` | `10.0.0.1:443` | `   203.0.113.7   , 192.0.2.10` | `true` | `203.0.113.7` |
| `XFF_Enabled_Absent_FallsBackToRemoteAddr` | `192.0.2.5:54321` | (none) | `true` | `192.0.2.5` |
| `XFF_Enabled_Empty_FallsBackToRemoteAddr` | `192.0.2.5:54321` | `` (empty string) | `true` | `192.0.2.5` |
| `XFF_Enabled_WhitespaceOnlyFirstEntry_FallsBackToRemoteAddr` | `192.0.2.5:54321` | `   , 198.51.100.4` | `true` | `192.0.2.5` |
| `XFF_Enabled_RemoteAddrAlsoMalformed_ReturnsEmpty` | `garbage` | `` (empty string) | `true` | `""` |

Notes:

- The `XFF_Enabled_WhitespaceOnlyFirstEntry_FallsBackToRemoteAddr` row enforces the "no non-empty entry → fallback" branch explicitly. Without it, an attacker that sends `X-Forwarded-For:   ,` could starve the fallback if the implementation only checked the trimmed first byte. The table catches that.
- The `XFF_Enabled_RemoteAddrAlsoMalformed_ReturnsEmpty` row enforces the bottom-of-flow `""` return when both sources fail. This is the "no usable source" AC line.
- IPv4-in-IPv6 forms (e.g. `[::ffff:192.0.2.5]:54321`) are NOT in the table — `net.SplitHostPort` handles them and the helper returns the host portion as-is per the no-canonicalisation contract. Adding a row would test `net.SplitHostPort`, not the helper. Out of scope.
- Tests construct requests via `httptest.NewRequest(http.MethodGet, "/", nil)` then set `r.RemoteAddr` and `r.Header` directly. No `httptest.NewServer`.

`make test -race`, `make vet`, `make build` must remain clean per repo norm.

### What this ticket does NOT do

- **No wiring change to `cmd/pyrycode-relay/main.go`.** Stated in the AC; the helper is unused at end-of-ticket. This is intentional and is `go vet`-clean because the function is exported.
- **No move of the existing `remoteHost` helper.** It stays unexported, stays in `server_endpoint.go`, stays as-is. Migrating logging to the new helper is the wiring ticket's call (it may decide `remoteHost`'s fallback-to-verbatim is correct for logging and leave it alone).
- **No per-IP rate-limit type.** Sibling ticket (#50 or its descendant). This ticket is the primitive only.
- **No trusted-proxy CIDR list.** Future ticket. Today's bool `trustForwardedFor` is a flat all-or-nothing trust signal.
- **No `X-Real-IP` support.** XFF is the AC's named header. `X-Real-IP` is a separate (and more proxy-specific) convention; it can be added in the wiring ticket once a concrete proxy is chosen.
- **No multi-header XFF concatenation.** See § *Open questions*.

### Open questions

- **Multi-header `X-Forwarded-For`.** If a request carries two `X-Forwarded-For:` headers, `r.Header.Get` returns only the first. RFC 7239 (the standardised `Forwarded` header) supersedes the de-facto XFF behaviour, and most real proxies emit a single comma-joined `X-Forwarded-For`. The de-facto-correct treatment for two-header XFF is to join them with commas before taking the left-most entry; the relay does NOT do this today and the AC does not name the case. **Resolution:** defer to the wiring ticket. The current behaviour (take left-most of first header) is documented in the docstring and tested in the single-header row. If the wiring ticket discovers a real proxy that emits two-header XFF, the resolution is straightforward (`r.Header.Values("X-Forwarded-For")` + `strings.Join`); the change is two lines and one new test row.

---

## Security review

**Verdict:** PASS

**Findings:**

- [Trust boundaries] No findings — the trust boundary is explicit: a single `bool` parameter on a single exported function. The helper itself performs **no** trust decision; the caller passes the bit it has determined from operator configuration. The docstring names the consequence ("XFF is attacker-controlled and must be ignored" in direct-facing deployments) so a developer reading the call site sees the threat without leaving the helper. The wiring ticket (#50 / sibling) owns the policy of where the bit comes from; this spec deliberately does not pre-commit it (flag vs env vs config-file is a follow-up decision).
- [Tokens, secrets, credentials] N/A by design — the helper handles a single string (IP) that is not a secret. Logging considerations are downstream.
- [File operations] N/A — no I/O.
- [Subprocess / external command execution] N/A — pure function.
- [Cryptographic primitives] N/A — no randomness, no comparison against a secret.
- [Network & I/O] No findings — the helper reads two fields of an already-parsed `*http.Request`. It does not call `Read`, set timeouts, or open sockets. No untrusted bytes are consumed in unbounded form: `r.Header.Get` returns the header value as parsed by `net/http`, which enforces its own header-size cap (`http.Server.MaxHeaderBytes`, default 1 MiB) at upgrade time. The helper performs no length check of its own because the upstream bound is already in place at the only call site that will ever exist (`cmd/pyrycode-relay/main.go` runs a single `http.Server` instance whose `MaxHeaderBytes` is the stdlib default; if the relay ever raises that, this helper's per-call cost is `O(len(XFF))` for the `IndexByte` + `TrimSpace` scan — linear, no allocation beyond the returned substring's header span).
- [Error messages, logs, telemetry] No findings — the helper does not log or wrap errors. The empty-string return is the only side-channel; the caller decides whether to log on `""`. The threat-model § *Log hygiene* "MAY be logged: remote host (IP)" line authorises logging the result. The helper does NOT log the **input** (request headers), which is the part with token-bearing risk.
- [Concurrency] N/A — pure function; the `*http.Request` is owned by the handler goroutine.
- [Threat model alignment] No findings — this primitive directly feeds the threat-model's § *DoS resistance — connection floods* "Future hardening: token-bucket rate limit" line. The split (primitive here, wiring next) is deliberate so the trust-model nuance gets its own review pass.

**Adversarial walk — what a hostile actor could try:**

1. **Header injection via XFF.** Adversarial phone sends `X-Forwarded-For: 127.0.0.1` to forge a loopback identity. **Defence:** the caller must pass `trustForwardedFor=false` in any internet-facing deployment. The docstring is explicit; the wiring ticket enforces the default. If the operator misconfigures the bit, every request reports `127.0.0.1` as its source and the rate-limit collapses to a single bucket — a self-DoS, not a privilege escalation. Acceptable as a configuration-error surface; the threat-model entry for log hygiene has the same shape (operator-owned).
2. **Header injection via spoofed XFF when proxy IS trusted.** Phone sets `X-Forwarded-For: victim-ip` before reaching the proxy. **Defence:** trusted proxies overwrite (or append to) XFF; an upstream-set value is the proxy's responsibility to sanitise. The relay-side helper cannot defend against a misconfigured proxy. Documented in the docstring ("only when the relay is fronted by a reverse proxy the operator trusts to set XFF correctly"). The wiring ticket should call this out in operator docs; here we ship the primitive.
3. **Whitespace bombing.** XFF set to a 1 MiB string of spaces. **Defence:** the helper does `IndexByte(',')` first (finds no comma → returns the full string), then `TrimSpace` on the result. `TrimSpace` is O(n) and allocation-free for a fully-whitespace string (returns the empty substring slice). The fallback-to-RemoteAddr branch triggers. `http.Server.MaxHeaderBytes` caps the input at 1 MiB so the linear scan is bounded. No amplification.
4. **Pathological comma.** XFF set to `,` (just a comma). `IndexByte` finds index 0; `v = v[:0]` is the empty string; `TrimSpace` is `""`; fallback to `RemoteAddr`. Behaves identically to the absent-header case.
5. **Embedded null bytes / control chars.** XFF set to `203.0.113.7\x00.attacker.example`. The helper returns the byte sequence up to the first comma (or the whole header if none), then trims ASCII whitespace. **It does not sanitise control bytes.** The threat is downstream: a rate-limit key with embedded nulls is fine (Go strings are byte-safe); a log line including the unsanitised string could confuse a log parser. **Resolution:** the threat-model § *Log hygiene* names log injection as a known concern; the wiring ticket's logging decision must `strconv.Quote` or similar before emit. This helper deliberately does not strip control bytes because (a) doing so would change the semantics of the returned key (two different attackers' nulls collapse to the same bucket) and (b) it would push log-safety policy into a security primitive. Documented under § *Out of scope*. **SHOULD FIX, deferred to the wiring ticket** — the wiring ticket must wrap log calls with quoting if it emits the IP string. Adding a `[clientip] note: caller must quote on log emit` line to the docstring is a free belt-and-suspenders move.
6. **RemoteAddr without a port.** Some net stacks (test fixtures, custom servers) set `r.RemoteAddr` to a bare IP. `net.SplitHostPort("192.0.2.5")` returns an error; the helper returns `""`. The rate-limit caller denies. **Acceptable** because (a) the stdlib `http.Server` always sets `host:port` for real sockets and (b) tests that construct `httptest.NewRequest` must set `RemoteAddr` explicitly to a `host:port` form (the test table enforces this).
7. **RemoteAddr empty.** Same path as (6). Empty-string return; deny. Tested.
8. **IPv6 zone-id.** `r.RemoteAddr = [fe80::1%eth0]:443`. `net.SplitHostPort` returns `fe80::1%eth0`; the helper preserves it. The rate-limit caller uses the full string as the bucket key. **Acceptable**: zone-id is part of the address per RFC 4007 and two different zones legitimately denote different interfaces; collapsing them is the wrong default. The docstring's "no canonicalisation" line names this.

Per § *Decision*: no MUST FIX. One SHOULD FIX (log-quoting) flagged in finding (5), deferred to the wiring ticket with the resolution path stated.

**Reviewer:** architect (self-review per `architect/security-review.md`)
**Date:** 2026-05-11
