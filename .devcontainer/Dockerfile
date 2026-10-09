# syntax=docker/dockerfile:1

# Base: golang:1.26.6-bookworm, not the Microsoft devcontainers Go image.
#
# `mcr.microsoft.com/devcontainers/go:1-1.26-bookworm` does not exist — that
# registry is on its 2.x line and tags as <image>-<go-minor>-<distro>
# (`2.1-1.26-bookworm`), which floats the Go PATCH. Floating the patch is the
# one thing this repo cannot tolerate: go.mod pins `toolchain go1.26.6`, so a
# base shipping an older patch makes every `go` invocation download a second
# toolchain at first use, and AGENTS.md pins 1.26.6 because earlier 1.26
# patches carry three reachable stdlib advisories `govulncheck` fails on.
# golang:1.26.6-bookworm is exactly that patch, so `go version` in here is the
# version the gates run against. The price is doing the devcontainer chores by
# hand — non-root user, sudo, GOPATH ownership — which is the rest of this file.
FROM golang:1.26.6-bookworm

# BuildKit populates this. Declaring it keeps one Dockerfile working on both
# amd64 and arm64 hosts; every download below is parameterised on it, and an
# empty value (classic builder) fails loudly in the case statements rather than
# fetching the wrong binary.
ARG TARGETARCH

# Pins live together at the top so a bump is one line and one cache miss.
# No floating tag and no `curl | sh` installer that resolves "latest" at build
# time: an image that builds differently tomorrow is not an environment.
ARG TASK_VERSION=v3.53.1
ARG UV_VERSION=0.12.9
# kubectl tracks the 1.35 line the kind cluster runs, and carries the
# kustomize the manifests are assembled with (`kubectl version --client`
# prints both, which is why that is the verification command below).
ARG KUBECTL_VERSION=v1.35.8
ARG KIND_VERSION=v0.33.0

ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=1000

# bash so `set -o pipefail` below is honoured; under /bin/sh a failing producer
# in a pipeline is silently discarded and a truncated download would install.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# git, curl and ca-certificates already ship in the golang base
# (buildpack-deps:bookworm-scm), so only the gaps are installed here.
#
# sudo is not a convenience. Devcontainer Features run their runtime hooks as
# the remote user, and docker-outside-of-docker's socket-permission fixup
# shells out to sudo; without it the mounted socket is unusable and kind cannot
# create a cluster. Passwordless is deliberate and scoped to a development
# image that holds no credentials and is never deployed.
#
# The base leaves GOPATH=/go owned by root while postCreateCommand runs
# `go mod download` as ${USERNAME}, so ownership is handed over here. The chown
# alone is not the guarantee, though, and must not be read as one: the
# devcontainer CLI's updateRemoteUserUID renumbers ${USERNAME} to the host
# developer's uid and chowns that account's HOME — not GOPATH — so on any host
# whose uid is not ${USER_UID} the renumbered account no longer owns /go. The
# mode bits are what actually hold. The base already leaves /go, /go/bin and
# /go/src world-writable with the sticky bit; the directories created here must
# match, or the first `go mod download` fails EACCES. Verified: uid 1234 in
# this image could not write /go/pkg/mod at 0755.
#
# GOCACHE moves into the same tree for the same reason one level further out.
# It defaults under HOME, and a container started with `docker run -u <uid>`
# has no passwd entry, so HOME is `/` and `go build` dies on
# "failed to initialize build cache at /.cache/go-build". Under /go both caches
# are writable by whatever uid the container ends up running as.
#
# This holds because the image bakes no modules: both caches ship empty and the
# first uid to build populates them under its own ownership. A `go mod
# download` added to this Dockerfile would run as root and leave a root-owned
# 0755 /go/pkg/mod/cache behind, which is the exact path the old image failed
# to create as uid 1234.
#
# The profile.d fragment is load-bearing, not tidiness. The golang base puts
# /usr/local/go/bin and /go/bin on PATH with ENV, but a LOGIN shell — which is
# what an editor terminal and `bash -lc` both start — re-sets PATH from
# /etc/profile and drops them. Without this, `go` resolves in `docker exec` and
# not in the terminal a developer actually types into. Verified by regression:
# `bash -lc 'go version'` returned 127 before this line existed.
RUN set -euo pipefail; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        sudo \
        less; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd --gid "${USER_GID}" "${USERNAME}"; \
    useradd --uid "${USER_UID}" --gid "${USER_GID}" --shell /bin/bash \
        --create-home "${USERNAME}"; \
    echo "${USERNAME} ALL=(root) NOPASSWD:ALL" > "/etc/sudoers.d/${USERNAME}"; \
    chmod 0440 "/etc/sudoers.d/${USERNAME}"; \
    printf 'export PATH="/go/bin:/usr/local/go/bin:${PATH}"\n' \
        > /etc/profile.d/10-golang-path.sh; \
    chmod 0644 /etc/profile.d/10-golang-path.sh; \
    mkdir -p /go/pkg/mod /go/bin /go/cache; \
    chown -R "${USER_UID}:${USER_GID}" /go; \
    chmod 1777 /go/pkg /go/pkg/mod /go/cache

ENV GOCACHE=/go/cache

# Task, uv and the cluster tools are the four things this repo's workflow needs
# that `go tool` does NOT resolve. Everything else (golangci-lint, gosec,
# govulncheck, modernize, gitleaks) is pinned in go.mod and needs no install.
#
# Each tool is fetched at a pinned version, checked against the publisher's
# published sha256, and then run with --version in the SAME layer. A pin that
# no longer exists, an asset renamed upstream or a truncated transfer therefore
# fails `docker build`, not the developer's first command an hour later.
# The checksum proves transfer integrity, not provenance — the version pin is
# what makes the build reproducible.

# Task: the Taskfile is the entrypoint to every gate (`task check`).
RUN set -euo pipefail; \
    tmp="$(mktemp -d)"; cd "${tmp}"; \
    asset="task_linux_${TARGETARCH}.tar.gz"; \
    base="https://github.com/go-task/task/releases/download/${TASK_VERSION}"; \
    curl -fsSLO "${base}/${asset}"; \
    curl -fsSLO "${base}/task_checksums.txt"; \
    grep " ${asset}$" task_checksums.txt | sha256sum -c -; \
    tar -xzf "${asset}" task; \
    install -m 0755 task /usr/local/bin/task; \
    cd /; rm -rf "${tmp}"; \
    task --version

# uv: only here to provide uvx, which is how the repo runs pre-commit. Nothing
# installs pre-commit itself — `uvx pre-commit` resolves it per invocation, so
# there is no second pinned copy to drift from .pre-commit-config.yaml.
RUN set -euo pipefail; \
    case "${TARGETARCH}" in \
        amd64) triple="x86_64-unknown-linux-gnu" ;; \
        arm64) triple="aarch64-unknown-linux-gnu" ;; \
        *) echo "unsupported TARGETARCH: '${TARGETARCH}'" >&2; exit 1 ;; \
    esac; \
    tmp="$(mktemp -d)"; cd "${tmp}"; \
    asset="uv-${triple}.tar.gz"; \
    base="https://github.com/astral-sh/uv/releases/download/${UV_VERSION}"; \
    curl -fsSLO "${base}/${asset}"; \
    curl -fsSLO "${base}/${asset}.sha256"; \
    sha256sum -c "${asset}.sha256"; \
    tar -xzf "${asset}" --strip-components=1; \
    install -m 0755 uv uvx /usr/local/bin/; \
    cd /; rm -rf "${tmp}"; \
    uv --version; \
    uvx --version

# kubectl and kind: the point of this branch is the local kind stack, and a dev
# container that cannot drive it is a generic Go box. helm is deliberately
# absent — the stack is plain manifests assembled with kubectl's built-in
# kustomize, and shipping helm would advertise an option the manifests do not
# support.
#
# dl.k8s.io publishes a bare hash with no filename, so the checksum line is
# assembled here; GitHub's is already in sha256sum(1) format.
RUN set -euo pipefail; \
    tmp="$(mktemp -d)"; cd "${tmp}"; \
    kbase="https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}"; \
    curl -fsSLO "${kbase}/kubectl"; \
    curl -fsSLO "${kbase}/kubectl.sha256"; \
    echo "$(cat kubectl.sha256)  kubectl" | sha256sum -c -; \
    install -m 0755 kubectl /usr/local/bin/kubectl; \
    kind_asset="kind-linux-${TARGETARCH}"; \
    gbase="https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}"; \
    curl -fsSLO "${gbase}/${kind_asset}"; \
    curl -fsSLO "${gbase}/${kind_asset}.sha256sum"; \
    sha256sum -c "${kind_asset}.sha256sum"; \
    install -m 0755 "${kind_asset}" /usr/local/bin/kind; \
    cd /; rm -rf "${tmp}"; \
    kubectl version --client; \
    kind --version

# No USER instruction, deliberately. The devcontainer CLI installs Features and
# runs the docker-outside-of-docker entrypoint as the container user, and that
# entrypoint fixes the socket's group ownership before handing over; keeping
# root as the container user is the contract those Features are tested against.
# `remoteUser` in devcontainer.json puts every interactive shell, task and
# postCreateCommand on ${USERNAME} instead, which is the account that owns /go.
