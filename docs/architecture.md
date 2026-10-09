# Architecture

## The shape

Two binaries:

```
cmd/herdr-agentisan ─→ internal/cli    (cobra tree herdr invokes from the manifest)
                              └─→ internal/<domain> packages (the rules)
cmd/devctl          ─→ internal/devctl (the repository's own gates)
```

`cmd/*` packages are thin shims — config, logging, tracing, signal handling,
fang styling. No logic. That is what keeps them outside the coverage
denominator honest: there is nothing in them worth testing that running the
binary does not already prove.

The load-bearing decision is that **rules live in domain packages under
`internal/`, not in commands**. herdr reaches the plugin through several
doors — actions, event hooks, startup hooks, panes — and each is a command;
a rule written into one of them is a rule the others do not have.

## How herdr runs the plugin

herdr reads `herdr-plugin.toml` and, for each action, execs one argv array
with the plugin root as the working directory and no shell. The process gets
its context through environment variables (`internal/plugin.EnvFrom`) and
talks back over herdr's unix socket (`internal/herdr`): newline-delimited
JSON, one request per connection. Every call is bounded by
`plugin.CallTimeout` (5s), because the process context carries no deadline
of its own and a herdr that accepts and never answers would otherwise hold
one of its in-flight action slots open indefinitely.

Two properties are enforced rather than hoped for:

- **The manifest matches the binary.** `herdr plugin link` runs no build and
  herdr only warns about a bad manifest, so `TestManifestMatchesTheBinary`
  checks that every action names a runnable cobra command exactly and execs
  the path the build writes.
- **Tests never reach a live herdr.** A developer's shell inside herdr has
  `HERDR_SOCKET_PATH` set. Commands read the environment through an injected
  lookup, and tests dial `herdrtest`, an in-process fake socket.

## Why two CLIs

`herdr-agentisan` is the product. `devctl` is the repository's own tooling, and it
exists because three of this repo's gates have no Go builtin:

- **Coverage floor.** `go test` has no `--cov-fail-under`. `devctl coverage`
  parses `go tool cover -func` and fails below the threshold.
- **Docs parity.** `devctl docs-parity` proves AGENTS.md, `Taskfile.yml` and
  `ci.yml` describe the same gates, and that no escape-hatch entry in
  `internal/devctl/parity.go` outlives the task or row it exempted.
- **Large files.** `devctl large-files` fails on a tracked file above the size
  ceiling, which git itself never enforces.

Keeping these in Go rather than a shell or Python script means the repository
needs no second language to run its own checks, and the checks are themselves
tested.

## The drift guard

An agent's most expensive failure mode here is not writing a bug — it is
trusting a document that has quietly stopped being true. `devctl docs-parity`
exists specifically to make that impossible: a documented command that CI no
longer runs would otherwise be run by an agent, seen to pass, and taken as
proof the gate passes.

It was verified by deliberately breaking it and watching it fail before being
trusted — a check that has never been seen failing is not evidence of anything.

## Configuration

`internal/config` resolves flags > environment > `.env` > defaults, and
validates. No secret has a default; when one is added, `config.Load()` must
fail at startup rather than fall back. Failing early is the design, not a
rough edge.

## Deliberate limits

- **Tracing exports only when told to.** `HERDR_AGENTISAN_OTLP_ENDPOINT` is empty by
  default, so `task run:cli` on a laptop records no spans and needs no
  collector. W3C propagation is installed either way, so a trace context still
  passes through to the next hop.
