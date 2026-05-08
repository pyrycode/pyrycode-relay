# ADR-0002: Explicit-failure 404 on `:80` instead of HTTPS redirect

**Status:** Accepted (#9)
**Date:** 2026-05-08

## Context

In `--domain` mode the relay binds `:80` for the ACME http-01 challenge. Anything *other* than an ACME challenge request hitting that port is, by definition, a misconfigured client — phones and binaries are required to use WSS on `:443`. The question is how to respond to those non-challenge requests.

`golang.org/x/crypto/acme/autocert.Manager.HTTPHandler(fallback)` defines the behaviour for non-challenge traffic:

- `fallback == nil` → autocert redirects GET/HEAD to HTTPS with `302` and returns `400` for other methods.
- `fallback != nil` → autocert calls `fallback.ServeHTTP` for all non-challenge traffic.

The ticket's literal AC text said "the `nil` argument means non-challenge HTTP requests get a 404," which is incorrect about autocert's behaviour. The intent stated alongside the AC was the load-bearing part: *"we do NOT redirect to HTTPS — we want explicit-failure semantics for misconfigured clients."*

## Decision

Pass `http.NotFoundHandler()` explicitly to `manager.HTTPHandler`, not `nil`. Non-challenge port-80 traffic returns `404` with no body. ACME challenges still flow through autocert's own routing and reach the cert-issuance handler unchanged.

## Rationale

A `302` redirect is convenient for browsers but actively unhelpful for the relay's clients:

- Mobile clients (phones, binaries) speak WSS on `:443`. They have no business reaching the relay over plain HTTP. A redirect would mask the misconfiguration; a `404` surfaces it.
- Browsers shouldn't reach the relay at all — there is no HTTP UI. A `404` is the honest answer.
- The relay's posture elsewhere (`--insecure-listen` vs `--domain` are mutually exclusive and explicit; `ErrCacheDirInsecure` refuses to start instead of silently weakening) is consistent loud-failure. Redirecting plain-HTTP traffic to HTTPS would be the lone silent-correction in this binary.

Returning a body in the 404 was considered and rejected — the relay leaks no internal state in error responses anywhere else, and nothing useful would go in the body.

## Consequences

- The wiring in `cmd/pyrycode-relay/main.go` is `mgr.HTTPHandler(http.NotFoundHandler())`, with a comment explaining why `nil` is wrong here. Future contributors who "clean up" to `nil` reintroduce the redirect.
- If a future operator deployment ever wants the redirect (e.g. fronting a browser-facing healthcheck on `:80`), this is a one-line change — but it should require a fresh ticket and decision.
- The choice is independent of the `:443` `EnforceHost` middleware (which returns `421` for Host-header mismatches). Both follow the same explicit-failure principle but address different threats: `EnforceHost` pins the application-layer host on TLS connections; the `:80` 404 prevents silent scheme-switching.
