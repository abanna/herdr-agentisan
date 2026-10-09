# Multi-stage: build with the full toolchain, ship a slim Debian runtime.
#
# Both stages are Debian. The builder was Alpine, which worked only because
# CGO is off — a static binary does not care what libc built it. Mixing them
# is still a smell: the day someone needs cgo, an Alpine-built binary against a
# glibc runtime fails at exec with no useful message.
#
# There is no HEALTHCHECK instruction. Kubernetes ignores it entirely and runs
# the probes in infrastructure/local/k8s; adding one would put a second, unread definition of
# "healthy" in the repo.

FROM golang:1.26.6-bookworm AS build

WORKDIR /src

# Copy manifests first so the module download layer caches independently of
# source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Provenance is passed in rather than derived: the build context has no .git,
# so debug.ReadBuildInfo would stamp nothing. Taskfile supplies these.
ARG VERSION=dev
ARG COMMIT=""

# CGO off for a static binary the distroless base can run.
# -trimpath strips local paths; -s -w drop the symbol table and DWARF.
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/nerds-run/go-agents/internal/config.Version=${VERSION} \
        -X github.com/nerds-run/go-agents/internal/config.Commit=${COMMIT}" \
      -o /out/server ./cmd/server

FROM debian:trixie-slim

# Debian, not distroless. The trade is deliberate and it is a real trade: this
# image HAS a shell and a package manager, which distroless removed on purpose,
# and it is ~110MB rather than ~30MB. What it buys is a glibc userland you can
# actually get into with `kubectl exec` during an incident, and one fewer
# surprise of the kind the Grafana image produced here — a musl/glibc-compat
# runtime resolving DNS somewhere other than the pod's resolv.conf.
#
# ca-certificates is the one package that is not optional: the OTLP exporter
# talks plaintext to an in-cluster collector today, but the moment that endpoint
# crosses a network boundary it needs a trust store, and discovering that at
# runtime is worse than 200KB now.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd --gid 65532 nonroot; \
    useradd --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin nonroot

# NUMERIC, not `USER nonroot`. The kubelet enforces runAsNonRoot before the
# container filesystem is readable, so it cannot resolve a name to a uid — a
# named USER makes it fall back to the manifest's runAsUser and the two can
# then drift apart silently. `devctl manifests` compares this number against
# deploy-time runAsUser and fails when they disagree.
USER 65532:65532

COPY --from=build /out/server /usr/local/bin/server

EXPOSE 8080 9090

ENTRYPOINT ["/usr/local/bin/server"]
