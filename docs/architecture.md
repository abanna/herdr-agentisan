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

## GitHub notice capture IDs (B0a.1)

`store.GitHubNoticeCaptureID` is a pure operation over frozen `NoticeEnvelope`
and `NoticeReceipt` values. It preserves inputs, ignores the caller's CaptureID
entirely and returns `gh1:` plus lowercase SHA256 hex, or a wrapped ErrInvalid
without input contents. It reads no state, clock or environment and has no
production callers. It allocates no capture identity, admits no record and
performs no board, seen, acknowledgement, delivery or wake effect.

Version 1 accepts only source github and merged/ci_failed/ci_passed/dirty/
approved/review, with a matching version-1 receipt and PR 1..2^31-1. Project,
subject and worker allow empty, otherwise UTF-8 without control/format characters,
at most 256 bytes. Text is nonempty UTF-8, at most 512 bytes, without NUL/CR/LF.
Repo has two nonempty components and one slash, at most 256 bytes, with the same
Unicode restrictions. OIDs are full 40/64 hex characters, preserving case;
approved keys are empty, and merge keys may be empty. Review keys are a nonempty
login (at most 128 bytes, no whitespace/control/format) plus one ASCII space and
a canonical UTC RFC3339Nano timestamp, together at most 256 bytes.

Encoding is four unsigned 64-bit big-endian words: envelope version, receipt
version, PR, then the signed nanosecond instant's two's-complement bits. The instant
uses the existing representable nanosecond range (1677..2262), rejecting Go zero
time, with UTC normalization. Next come unsigned 32-bit big-endian lengths and
bytes for project/source/kind/subject/worker/repo/key/text in that exact order.
CaptureID and result/retry/disposition data are absent. No text is rendered,
truncated or normalized. Length guards precede variable decoding/error allocation.
The 2-KiB encoding ceiling is enforced; a conservative valid bound is 32 numeric
bytes + 32 prefixes + five 256-byte fields + 512 text + 6 source + 9 kind = 1871,
so fully valid inputs cannot reach 2048. Tests cover reachable field boundaries.

Durable admission, supplied capture-ID validation, metadata and schema changes
remain B0a.2, after human merge and separate authorization. Board B is incomplete.

## Deliberate limits

- **Tracing exports only when told to.** `HERDR_AGENTISAN_OTLP_ENDPOINT` is empty by
  default, so `task run:cli` on a laptop records no spans and needs no
  collector. W3C propagation is installed either way, so a trace context still
  passes through to the next hop. The daemon's workflow events follow the same
  rule: no `[telemetry] endpoint` and no variable, nothing is built
  (`docs/telemetry.md`).
