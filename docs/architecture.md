# Architecture

## The shape

Two binaries, one domain:

```
cmd/go-agents ─→ internal/notes (domain: validation + Store interface)
                        ├─ MemStore   (in-process, used by tests)
                        └─ FileStore  (JSON on disk, used by the CLI)
cmd/devctl    ─→ internal/devcli (the repository's own gates)
```

`cmd/*` packages are thin shims — signal handling, fang styling, process
lifecycle. No logic. That is what keeps them outside the coverage denominator
honest: there is nothing in them worth testing that running the binary does
not already prove.

The load-bearing decision is that **rules live in `internal/notes`, not in
commands**. A validation rule added to `Draft.Validate` applies to every
caller at once. `notes.Store` is the seam: swapping the in-memory or file
store for something else means implementing four methods, not editing
commands.

## Why two CLIs

`go-agents` is the product. `devctl` is the repository's own tooling, and it
exists because three of this repo's gates have no Go builtin:

- **Coverage floor.** `go test` has no `--cov-fail-under`. `devctl coverage`
  parses `go tool cover -func` and fails below the threshold.
- **Docs parity.** `devctl docs-parity` proves AGENTS.md, `Taskfile.yml` and
  `ci.yml` describe the same gates, and that no escape-hatch entry in
  `internal/devcli/parity.go` outlives the task or row it exempted.
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

- **`MemStore` and `FileStore` are not production storage.** `FileStore`
  rewrites the whole file under a mutex; correct for one developer on one
  machine, and honest about its ceiling. Concurrent writers across processes
  would need a lock file or a database.
- **Tracing exports only when told to.** `GO_AGENTS_OTLP_ENDPOINT` is empty by
  default, so `task run:cli` on a laptop records no spans and needs no
  collector. W3C propagation is installed either way, so a trace context still
  passes through to the next hop.
