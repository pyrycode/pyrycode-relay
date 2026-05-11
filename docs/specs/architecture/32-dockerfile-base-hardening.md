# Spec: Dockerfile + base hardening — portable deploy artifact (#32)

## Files to read first

- `cmd/pyrycode-relay/main.go:20-35` — `Version` variable and `--version` flag; the no-network startup form used to verify the image runs.
- `cmd/pyrycode-relay/main.go:40-43` — required-flag check; explains why `pyrycode-relay` with no args exits `2` and `--version` is the only flagless success path.
- `Makefile:6-13` — `PKG`, `LDFLAGS`, and the `-X main.Version=$(VERSION)` injection the Dockerfile mirrors.
- `go.mod:1-3` — module path (`github.com/pyrycode/pyrycode-relay`) and Go toolchain version (`go 1.26.2`); the build stage tag tracks this.
- `README.md` § *Build* and § *Run* — the docs to extend with a *Docker* subsection.
- `docs/threat-model.md` § *Deploy security — VPS compromise*, § *Supply chain — Go dependencies*, § *Cert & key handling* — the three operational surfaces this artifact contributes hardening to. Used to scope the spec's security review.
- `docs/PROJECT-MEMORY.md` § *Patterns established* — for tone and convention (loud failure, deliberate dependency surface, policy at the wiring site).

## Context

The relay has no Dockerfile today. Without one, it can't ship to any platform that expects an OCI image. This ticket delivers **only the portable artifact** — host-agnostic, deliberately stripped, with hardening baked in at build time. Host-specific wiring (TLS termination, port mapping, volume backing, single-instance constraint, healthcheck wiring) lives in #38 / #39 / #42.

The relay is internet-exposed. Image-layer hardening (small base, no shell, no package manager, non-root, digest-pinned bases, stripped binary) is defence-in-depth on top of the runtime hardening already in place (autocert cache permission check, slow-loris timeouts, header-gate before WS upgrade, 256 KiB frame cap, opaque payload routing).

## Design

### Files

1. **`Dockerfile`** (new, repo root) — multi-stage: `build` → `runtime`.
2. **`.dockerignore`** (new, repo root) — minimal, excludes `.git`, build outputs, and the Dockerfile itself from the build context. Justification under § *On `.dockerignore`* below.
3. **`README.md`** — new *Docker* subsection under *Build*; ~10 lines.

No Go code changes. No new types or interfaces.

### Stage 1: build

```dockerfile
# syntax=docker/dockerfile:1.7

# Tracks: golang:1.26-bookworm
# Pinned by digest so a tag-only swap upstream can't shift what we build.
# Renovate keeps the digest fresh; replace the placeholder below with the
# current digest at implementation time (see § "Pinning the digests").
FROM golang:1.26-bookworm@sha256:<PLACEHOLDER> AS build

ARG VERSION=dev

WORKDIR /src

# Pull module deps first so source-only changes don't bust the layer cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a fully static binary that runs on
# distroless/static (no glibc on the runtime image). GOFLAGS unset.
# -trimpath strips host paths from the binary; -s -w strips the symbol
# table and DWARF. -X mirrors the Makefile's main.Version injection so
# `pyrycode-relay --version` reports something useful in containerised
# builds.
RUN CGO_ENABLED=0 GOOS=linux \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.Version=${VERSION}" \
      -o /out/pyrycode-relay \
      ./cmd/pyrycode-relay
```

### Stage 2: runtime

```dockerfile
# Tracks: gcr.io/distroless/static-debian12:nonroot
# Pinned by digest; see § "Pinning the digests".
FROM gcr.io/distroless/static-debian12:nonroot@sha256:<PLACEHOLDER>

# Belt-and-suspenders: the :nonroot variant already sets USER 65532
# upstream, but a future base swap could silently regress it. The
# explicit line guarantees the invariant survives any base-image change
# that doesn't also update this Dockerfile.
USER nonroot:nonroot

COPY --from=build --chown=nonroot:nonroot /out/pyrycode-relay /pyrycode-relay

# Documented mount point for the autocert cache. The host manifest (#38)
# bind-mounts this; without a mount, the directory does not exist inside
# the container and --cert-cache must be overridden by the host.
# Pre-creation with the correct ownership lives in the host manifest —
# distroless has no shell to mkdir here, and VOLUME with a missing dir
# behaves consistently for both bind- and anonymous-mount cases.
VOLUME ["/var/lib/relay/autocert"]

# 80: ACME http-01 challenge listener (autocert mode).
# 443: WSS listener (autocert mode).
# The portable artifact exposes both; the host manifest (#38) chooses
# whether TLS terminates here (publish both) or upstream of the relay
# (publish neither, set --insecure-listen instead).
EXPOSE 80 443

ENTRYPOINT ["/pyrycode-relay"]
```

### `VERSION` strategy: build arg, defaults to `dev`

The Dockerfile accepts `--build-arg VERSION=…` and injects it via the same `-X main.Version=…` ldflag the Makefile uses. Two reasons over "omit and accept `dev`":

- **Symmetry with `make build`.** A contributor inspecting `bin/pyrycode-relay` and `pyrycode-relay:dev` should see the same `--version` semantics. Diverging here would surprise them when release tooling lands.
- **Future-proofing for #38.** When release tooling sets the image tag, it'll already want to pass a real version through; baking the wiring in now means #38 doesn't need to touch this Dockerfile.

Cost: one `ARG` line + one term in the ldflag. Trivial.

### Pinning the digests

The AC requires both base images be pinned by `@sha256:…`. Tag selection drives which **upstream** the digest tracks; the digest itself is fetched at implementation time and refreshed by Renovate thereafter.

Developer steps (at implementation time, once per base):

```bash
docker pull golang:1.26-bookworm
docker inspect --format='{{index .RepoDigests 0}}' golang:1.26-bookworm
# → golang@sha256:<hex> — paste the hex into the Dockerfile.

docker pull gcr.io/distroless/static-debian12:nonroot
docker inspect --format='{{index .RepoDigests 0}}' gcr.io/distroless/static-debian12:nonroot
# → gcr.io/distroless/static-debian12@sha256:<hex>
```

Both digest lines carry a `# Tracks: <tag>` comment naming the upstream tag they were pinned from. Renovate (or a human reviewer) uses that comment to sanity-check what the digest *should* refer to when proposing a bump. This satisfies the AC's last bullet.

### Why distroless/static-debian12:nonroot

- **`static-debian12`** — no glibc, no apt, no shell, no package manager, no `/etc/passwd` games. Smallest attack surface available for a fully-static Go binary. Build stage uses `bookworm` (== debian12) so any C deps Go's toolchain pulls in match the static base's expectations; we still build with `CGO_ENABLED=0`, so this is belt-and-suspenders, not a hard requirement.
- **`:nonroot`** — runs as uid `65532`, gid `65532`, with `$HOME=/home/nonroot`. The relay's `defaultCertCache()` (`cmd/pyrycode-relay/main.go:120-125`) resolves to `/home/nonroot/.pyrycode-relay/certs` inside the container, but real deployments will pass `--cert-cache /var/lib/relay/autocert` (the documented mount point); the default is only relevant for `--version` smoke tests, which never touch the directory.

### On `.dockerignore`

The AC marks `.dockerignore` in-scope only if it materially affects build correctness. Including a minimal one keeps the build context bounded (the relay repo's `.git` directory is ~the largest item; `bin/` and `dist/` are present after a `make build` on the host). A bloated context isn't a correctness bug today, but it's adjacent — a memory-constrained CI runner could fail `docker build` with `.git` included and succeed without. Included for parity with the rest of the project's defensive defaults.

```
.git
bin/
dist/
coverage.txt
*.test
Dockerfile
.dockerignore
```

`docs/` is **not** excluded — `COPY . .` pulls it in, but the build stage only invokes `go build` against `./cmd/pyrycode-relay`, so the docs land in the build stage filesystem and are dropped at the multi-stage boundary. Excluding them is unnecessary and would surprise a contributor running `docker build` from the same checkout they edit docs in.

### README update

A new *Docker* subsection under *Build* in `README.md`:

```markdown
## Docker

```bash
docker build -t pyrycode-relay:dev .
docker run --rm pyrycode-relay:dev --version
```

The image is host-agnostic: it exposes `:80` and `:443` for autocert,
and mounts the autocert cache at `/var/lib/relay/autocert`. Host-specific
deploy wiring (TLS termination policy, port publishing, volume backing,
single-instance enforcement) lives in #38.
```

(Fenced-code markers are escaped in this spec; the README update uses real backticks.)

### Verification (developer's AC checklist)

1. `docker build -t pyrycode-relay:dev .` from a clean checkout completes successfully.
2. `docker run --rm pyrycode-relay:dev --version` prints `dev` and exits `0`. (The `flag` package treats `--help`/`-h` as a usage print and exits `2`; `--version` is the only zero-exit no-network startup form. The AC says "or equivalent" — `--version` is the equivalent.)
3. `docker inspect --format '{{.Config.User}}' pyrycode-relay:dev` prints `nonroot:nonroot` (or equivalent `65532:65532` — both are how distroless surfaces it depending on docker version). Distroless has no `id`/`whoami`, so image-inspection is the AC-permitted verification path.
4. `docker inspect --format '{{.Config.ExposedPorts}}'` lists `80/tcp` and `443/tcp`.
5. `docker inspect --format '{{.Config.Volumes}}'` lists `/var/lib/relay/autocert`.

These are AC verification steps, not test code — there is no test suite to extend.

## Concurrency model

Not applicable. The artifact is a single binary in a container; the binary's own concurrency model (already specified across #3, #7, #16, #21, #25, #26) is unchanged.

## Error handling

Not applicable at the Dockerfile level — `go build` either succeeds or the build fails. The runtime image inherits the binary's existing failure modes (autocert cache permission check, etc.); the Dockerfile does not introduce new ones.

## Testing strategy

No unit tests; the artifact is verified by the AC steps above. Two structural defences against future regression:

- **Belt-and-suspenders `USER` line.** A base-image swap that drops the upstream `USER 65532` would not silently regress us — the explicit line stays.
- **`# Tracks: <tag>` comment alongside each digest pin.** A reviewer (or Renovate) can spot a digest that no longer corresponds to its tracked tag without consulting external state.

Image-layer scanning (Trivy / Grype) lands in a separate ticket per the issue's *Out of scope* note.

## Open questions

- **Digest values at implementation time.** Resolved by the developer running `docker pull` + `docker inspect` (see § *Pinning the digests*). Not a design question; just a transient lookup.
- **Tag granularity (`1.26` vs `1.26.2`).** Spec picks `1.26` (minor): the digest pin makes byte-equivalence non-negotiable regardless of tag, and tracking the minor lets Renovate roll up patch bumps as digest-only changes instead of tag churn. If the developer prefers `1.26.2` for tighter human-readable provenance, that's an acceptable swap — the digest is the load-bearing part.

## Security review

The ticket carries the `security-sensitive` label. The pass below walks the spec against the adversarial-design categories that apply to a containerised internet-exposed relay. Performed before commit; verdict: PASS.

### Trust boundaries

- **Image build is build-time-trusted.** The Dockerfile runs `go build` over the working tree; the developer controls the inputs. No build-time network access beyond `go mod download` (already covered by `go.sum` integrity and the project's existing supply-chain posture).
- **Runtime trust boundary is unchanged from the host binary.** The container exposes `:80` and `:443`; the binary's existing header-gate, autocert cache check, and WS adapter caps remain the chokepoints. The Dockerfile adds no new code paths.

### Adversarial inputs

- **No new input surface.** The image does not introduce new listeners, flags, env vars, or filesystem paths the binary will read from. `/var/lib/relay/autocert` is declared via `VOLUME` but the binary only touches it if `--cert-cache` points there; the host manifest (#38) is responsible for that wiring.
- **`VOLUME` with no mount.** If the operator runs the image without bind- or anonymous-mounting the volume, the binary's `--cert-cache` default (`/home/nonroot/.pyrycode-relay/certs`) takes effect. Autocert will then create `/home/nonroot/.pyrycode-relay/certs` with `0700` on first start, satisfying the existing permission check in `internal/relay/tls.go`. This is a degraded posture (cache vanishes on container restart, forces re-issuance), but not a security regression — the host manifest closes it.

### Privilege

- **Runs as uid 65532, not 0.** Distroless `:nonroot` + explicit `USER nonroot:nonroot` belt-and-suspenders. A future contributor cannot accidentally drop the `USER` line and root the runtime, because the base also enforces it; both layers would need to regress simultaneously.
- **No `setuid`, no capabilities granted in the artifact.** Binding `:80` and `:443` from uid 65532 is the host's problem (port mapping, `CAP_NET_BIND_SERVICE`, or a host-side proxy); the portable artifact stays uid-neutral.

### Supply chain

- **Both bases digest-pinned.** A tag-swap attack on `golang` or `distroless/static` cannot change what we build against between Renovate bumps. The `# Tracks:` comment makes a malicious digest swap (changing the digest while leaving the tag comment unchanged) reviewable.
- **No `ADD` from URLs, no `RUN curl | sh`, no third-party scripts.** The only thing that enters the image is `go build` output.
- **No `apt-get install`, no `apk add`.** Distroless has neither; the build stage doesn't add packages.

### Data at rest in the image

- **No secrets baked in.** No copies of `.env`, no credentials, no `.git` (excluded by `.dockerignore`), no docs that might carry stray secrets (docs do enter the build stage but are dropped at the multi-stage boundary; they are not COPYed into the runtime image).
- **Binary stripped (`-s -w`).** No symbol table, no DWARF. Not a security boundary on its own, but reduces post-exploitation reverse-engineering convenience and signals build hygiene.
- **`-trimpath`.** Strips host paths from the binary; defends against accidental disclosure of build-host directory structure via panic stack traces.

### Logging / observability

- **No new log lines.** The Dockerfile does not change any logging behaviour; the existing log-hygiene rules in `docs/threat-model.md` § *Log hygiene* continue to apply unchanged.

### Failure modes

- **`docker build` failure** is loud and local; no security implication.
- **Missing digest at pin time** (developer pastes a placeholder) — `docker build` errors out with a manifest-fetch failure. Loud, local, no silent fallback.

### Verdict

**PASS.** The spec adds image-layer hardening without introducing new trust boundaries, new input surfaces, or new privilege paths. The runtime threat model is unchanged; the operational threat model (`docs/threat-model.md` § *Deploy security*, § *Supply chain*) gains defence-in-depth on small base, non-root, digest-pinned bases, and stripped binary.
