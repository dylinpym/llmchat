# syntax=docker/dockerfile:1

# Multi-stage build for the llmchat server.
#
# WHY CGO IS OFF (CGO_ENABLED=0)
#   The runtime stage is distroless/static — it contains no glibc and no C
#   runtime at all. A cgo-linked Go binary records a dynamic dependency on
#   libc/libpthread and would fail at exec with "not found" or
#   "GLIBC_x.y not found". Building with CGO_ENABLED=0 produces a fully static
#   binary using Go's pure-Go resolver, so it runs unchanged on a base image
#   that ships no libc. This project's dependencies (pgx/v5 is pure Go; the
#   standard net/http stack) have no reason to need cgo, so nothing is lost.
#   The side effect — one static ELF that is tiny and portable — is the point.

# --------------------------------------------------------------------- build
FROM golang:1.24-alpine AS build

# git is only needed if a dependency is fetched from a VCS rather than the
# module proxy; keep it so private/proxy-less builds still resolve.
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Copy the module files first so `go mod download` is its own cache layer and
# is not busted every time a source file changes.
COPY go.mod go.sum ./
RUN go mod download

# Now the rest of the source.
COPY . .

# Build the server binary.
#   CGO_ENABLED=0  -> static binary, no libc (required for distroless/static).
#   GOFLAGS=-trimpath -> strips local filesystem paths from the binary.
#   -ldflags "-s -w -buildid=":
#       -s  omit the symbol table
#       -w  omit the DWARF debug info
#       -buildid=  blank the build id, which makes the output reproducible.
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOFLAGS=-trimpath
RUN go build \
      -trimpath \
      -ldflags="-s -w -buildid=" \
      -o /out/server \
      ./cmd/server

# --------------------------------------------------------------- runtime
# distroless/static: nonroot is the smallest, most auditable option and is what
# makes CGO_ENABLED=0 mandatory (see header). :nonroot ships a fixed non-root
# uid, so no adduser step or /etc/passwd edit is needed.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# Distroless has no shell and no package manager: do not try to `run` commands
# for debugging — use `docker compose run --entrypoint /server server` or a
# throwaway sidecar instead.

WORKDIR /app

# The binary was built statically, so CA certs live in the image, not the binary.
# distroless/static already carries /etc/ssl/certs, so TLS to the LLM gateway
# works without copying ca-certificates from the build stage.

# goose runs its migrations against these files at startup; without them the
# server cannot apply or verify schema, so they must ship in the image.
COPY --chown=nonroot:nonroot migrations ./migrations

# The statically linked server, owned by nonroot and executed as uid 65532.
COPY --from=build --chown=nonroot:nonroot /out/server /app/server

# The image runs as nonroot already (the :nonroot variant sets User 65532);
# declare it explicitly so scanners and `docker inspect` agree.
USER 65532:65532

# Public API (ADDR) and internal metrics/health (INTERNAL_ADDR). These are the
# ports the app listens on INSIDE the container; docker-compose decides whether
# to publish any of them to the host (it publishes neither by default).
EXPOSE 8080 9090

# No healthcheck here: the distroless/static image has no shell or curl to run
# one with. Liveness/readiness belong to the orchestrator (compose `depends_on`
# healthcheck on postgres; k8s probes hit /healthz and /readyz).

ENTRYPOINT ["/app/server"]
