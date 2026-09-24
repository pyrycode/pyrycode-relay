# ADR-0011: Import `golang.org/x/oauth2/jwt` directly, not `golang.org/x/oauth2/google`

**Status:** Accepted (#132)
**Date:** 2026-09-24

## Context

The relay needs to mint short-lived OAuth access tokens from an operator-supplied
Google service-account JSON key, to authenticate FCM HTTP v1 sends
([FCM push sender](../features/fcm-push-sender.md)). `golang.org/x/oauth2` is a
new direct dependency — [ADR-0008](0008-prometheus-client-adoption.md)
established "an ADR file, before the `go.mod` edit" as the pattern for any next
direct dep, so this file is that ADR for `x/oauth2`.

The ticket's Technical Notes suggested `golang.org/x/oauth2/google`, whose
`JWTConfigFromJSON` does the credential parsing in one call: parse the key
JSON, check `type == "service_account"`, map `client_email` /
`private_key` / `private_key_id` / `token_uri` onto a `jwt.Config`, defaulting
the token URL. That convenience has a cost: `google` also imports
`cloud.google.com/go/compute/metadata`, `oauth2/authhandler`,
`oauth2/google/externalaccount`, `oauth2/google/internal/impersonate` and
`oauth2/google/internal/stsexchange` — none of which the service-account JWT
flow uses.

## Decision

Import only `golang.org/x/oauth2` and `golang.org/x/oauth2/jwt`. Do the JSON
mapping ourselves in `parseFCMCredentials` (`internal/relay/push.go`) instead
of calling `google.JWTConfigFromJSON`.

## Rationale

- **Smaller build graph.** The build only pulls packages from the
  `golang.org/x/oauth2` module itself: `oauth2`, `oauth2/internal`,
  `oauth2/jws`, `oauth2/jwt`. `go.sum` confirms this — the diff that added
  `x/oauth2 v0.37.0` added no `cloud.google.com/go/*` entries.
  `cloud.google.com/go/compute/metadata` is listed in `x/oauth2`'s own
  `go.mod` (because `google` imports it, for GCE-metadata-server credential
  discovery — irrelevant off-GCE) but is never compiled into the relay
  binary, because nothing on this path imports `google`.
- **No functionality lost.** Token caching and `token_uri` handling come from
  `jwt.Config.TokenSource` (wrapped in `oauth2.ReuseTokenSource`, caching
  until `Expiry - 10s`), not from `google`. The ticket's caching and
  `token_uri` requirements are satisfied identically either way.
- **Full control of the credential-error text.** AC 4 requires that a
  malformed service-account key fail with fixed, value-free error text (both
  from `NewFCMSender` and from the `envContracts` validator). Doing the
  parse ourselves means every failure path returns the bare
  `ErrFCMCredentials` sentinel; `JWTConfigFromJSON`'s own `json`/`x509`
  wrapped errors are not part of the surface we have to scrub.

## Consequences

- `parseFCMCredentials` re-implements the small mapping `JWTConfigFromJSON`
  would otherwise do, including replicating the private-key parse order
  (PEM, then PKCS#8, then PKCS#1) that `x/oauth2/internal.ParseKey` uses —
  that internal package cannot be imported directly, so the relay's copy
  (`isRSAPrivateKey`) must be kept in sync if a future `x/oauth2` release
  changes the accepted key encodings for the JWT signer.
- If a future ticket needs a broader Google credential flow (workload
  identity federation, GCE metadata-server credentials, impersonation), that
  ticket imports `google` at that point and this ADR's scope narrows to "the
  FCM sender specifically doesn't need it" rather than "the relay avoids
  `google` altogether."
- Supply-chain accounting for `x/oauth2` lives in
  [`docs/threat-model.md` § Supply chain — Go dependencies](../../threat-model.md).
