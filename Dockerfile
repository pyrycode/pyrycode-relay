# syntax=docker/dockerfile:1.7

# Tracks: golang:1.26-bookworm
# Pinned by digest so a tag-only swap upstream can't shift what we build
# against. Renovate keeps the digest fresh; refresh in lockstep with the
# tag comment so a future reviewer can sanity-check what the digest tracks.
FROM golang:1.26-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build

ARG VERSION=dev

WORKDIR /src

# Pull module deps first so source-only changes don't bust the layer cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a fully-static binary that runs on
# distroless/static (no glibc on the runtime image). -trimpath strips
# host paths from the binary; -s -w strips the symbol table and DWARF.
# -X mirrors the Makefile's main.Version injection so
# `pyrycode-relay --version` reports something useful in container builds.
RUN CGO_ENABLED=0 GOOS=linux \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.Version=${VERSION}" \
      -o /out/pyrycode-relay \
      ./cmd/pyrycode-relay

# Tracks: gcr.io/distroless/static-debian12:nonroot
# Pinned by digest; see the build-stage pin comment above for the rationale.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1

# Belt-and-suspenders: the :nonroot variant already sets USER 65532
# upstream, but a future base swap could silently regress it. The explicit
# line guarantees the invariant survives any base-image change that
# doesn't also update this Dockerfile.
USER nonroot:nonroot

COPY --from=build --chown=nonroot:nonroot /out/pyrycode-relay /pyrycode-relay

# Documented mount point for the autocert cache. The host manifest (#38)
# bind-mounts this; without a mount, the directory does not exist inside
# the container and --cert-cache must be overridden by the host.
VOLUME ["/var/lib/relay/autocert"]

# 80: ACME http-01 challenge listener (autocert mode).
# 443: WSS listener (autocert mode).
# The portable artifact exposes both; the host manifest (#38) chooses
# whether TLS terminates here (publish both) or upstream of the relay
# (publish neither, set --insecure-listen instead).
EXPOSE 80 443

ENTRYPOINT ["/pyrycode-relay"]
