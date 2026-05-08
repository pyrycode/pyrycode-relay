# Lessons

Gotchas worth carrying forward. Each entry: what bit us (or nearly did), and what we do about it.

## `json.Valid` rejects nil and empty bytes

`encoding/json.Valid(nil)` and `json.Valid([]byte{})` both return `false`. Useful: a single `if !json.Valid(frame)` check covers nil, empty, and malformed bytes — no need for separate length guards. Source: routing-envelope `Marshal` (#1).

## `json.RawMessage` of a JSON `null` is the 4-byte string `"null"`

When unmarshalling `{"frame": null}` into a struct with `Frame json.RawMessage`, the resulting `Frame` is `[]byte("null")` — length 4, *not* zero — so a length-only check treats it as present. If absence-vs-null matters semantically, also compare against `[]byte("null")` explicitly. Source: routing-envelope `Unmarshal` (#1).

## `json.RawMessage` round-trips are byte-stable *modulo whitespace*

`encoding/json` re-encodes `RawMessage` through the outer marshaller and may strip insignificant whitespace. Tests asserting opacity should canonicalise both sides with `json.Compact` before `bytes.Equal`, otherwise they flake on formatting. Source: routing-envelope tests (#1).

## Slice swap-and-truncate needs an explicit nil for GC hygiene

Removing an element from `[]Conn` (or any slice of pointer-bearing values) by `slice[i] = slice[len(slice)-1]; slice = slice[:len(slice)-1]` leaves the original last element reachable from the underlying array's spare capacity, pinning whatever it references in memory. Set `slice[len(slice)-1] = nil` *before* re-slicing. Source: `Registry.UnregisterPhone` (#3). Catches: connection handles can be heavy structs; a leaked `Conn` keeps a goroutine and a socket buffer alive.

## Race-test count is a CI-runner knob, not test code

The AC asked for `go test -race -count=20` coverage. `count` belongs on the command line, not as a loop inside the test. `make test` already runs `-race`; document the manual stress invocation in the test's doc comment so future contributors know how to reproduce locally. Source: `TestRegistry_RaceFreedom` (#3).

## A passive registry returns copies, callers do the slow work outside the lock

When a shared map holds connection handles (or anything you'll iterate to do I/O over), the read accessor should allocate and return a fresh copy. Callers can then `Send` / `Close` / fan out without holding the lock — and concurrent mutators don't trip the iteration. The cost is one allocation per call; the benefit is no foot-gun where a slow `Send` blocks every other registry caller. Source: `Registry.PhonesFor` (#3).

## `autocert.Manager.HTTPHandler(nil)` redirects to HTTPS — it does not 404

The autocert docs and the `nil` argument are easy to misread as "no fallback → 404." Actual behaviour: GET/HEAD get a `302` to HTTPS; other methods get `400`. To get an explicit 404 (or any other handler), pass it explicitly: `mgr.HTTPHandler(http.NotFoundHandler())`. See ADR-0002. Source: autocert TLS wiring (#9).

## TLS handshake SNI does not bind the HTTP `Host` header

A client can complete the TLS handshake using SNI for `relay.example.com` and then send `Host: somethingelse.com` in the HTTP request on the same connection. The wire spec says they must match for the relay; enforce it in application code (`EnforceHost` returns `421 Misdirected Request`). Don't assume autocert's `HostWhitelist` covers this — that gates ACME *issuance*, not request-time host validation. Source: autocert TLS (#9).

## `r.Host` may carry a `:port` suffix

RFC 7230 §5.4 allows it. When comparing to a configured hostname, strip the port via `net.SplitHostPort` first (it errors out cleanly if there's no port — use the original string in that case). Compare with `strings.EqualFold` because hostnames are case-insensitive. Source: `EnforceHost` (#9).

## `autocert.Manager.TLSConfig()` doesn't set `MinVersion`

`gosec` G402 fires on `make lint` if you use it raw. Wrap it in a helper that pins `MinVersion = tls.VersionTLS12` (or 1.3) before handing it to `http.Server`. Centralising the override means a future bump is a one-line change. Source: `relay.TLSConfig` (#9).
