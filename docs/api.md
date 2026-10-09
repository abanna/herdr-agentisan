# API

Base URL: `http://localhost:8080` by default (`GO_AGENTS_HTTP_ADDR`).

The machine-readable contract is [`openapi.json`](openapi.json), generated from
the bound routes by `go run ./cmd/devctl openapi --write` and drift-checked in
CI. **This page is prose; that file is the source of truth.** If the two
disagree, the JSON is right and this page needs updating.

## Authentication

Reads are open. Mutating routes require a bearer token when
`GO_AGENTS_API_TOKEN` is set:

```
Authorization: Bearer <GO_AGENTS_API_TOKEN>
```

The token is compared in constant time. It may only be empty when
`GO_AGENTS_ENV=development` — `config.Load()` refuses to start otherwise, so a
deployed service cannot accidentally run unauthenticated.

## Routes

| Method | Path | Auth | Success | Notes |
|--------|------|------|---------|-------|
| GET | `/healthz` | — | 200 | Liveness. Always 200 while the process runs. |
| GET | `/readyz` | — | 200 / 503 | Readiness. Touches the store, so a broken dependency shows here. |
| GET | `/version` | — | 200 | `{"version","commit"}` from the link-time build stamp. |
| GET | `/v1/notes` | — | 200 | Every note, newest first. Empty is `[]`, never `null`. |
| POST | `/v1/notes` | bearer | 201 | Returns the note and a `Location` header. |
| GET | `/v1/notes/{id}` | — | 200 / 404 | |
| DELETE | `/v1/notes/{id}` | bearer | 204 / 404 | Deleting twice is a 404, not a silent success. |

## Bodies

A note:

```json
{
  "id": "9406ceca-fd9b-435a-b2cb-92a3a60b18a9",
  "title": "two-pointer technique",
  "body": "sorted array pair sum",
  "created_at": "2026-09-03T18:22:44.123456789Z"
}
```

A draft (what you POST) is `title` and optional `body`. The server assigns
`id` and `created_at`; sending them is ignored.

Every failure returns the same shape:

```json
{ "error": "invalid note: title must not be empty" }
```

## Validation

`title` is required, trimmed, and 1–200 bytes after trimming — so a title of
only whitespace is rejected. `body` is optional and at most 4096 bytes. Both
limits are constants in `internal/notes`, shared with the CLI.

Internal errors are logged server-side and returned as a generic
`{"error":"internal error"}` with 500. The underlying message is never echoed
to a client.

## Examples

```bash
curl -s localhost:8080/healthz

curl -s -X POST localhost:8080/v1/notes \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $GO_AGENTS_API_TOKEN" \
  -d '{"title":"topological sort","body":"Kahn'\''s algorithm"}'

curl -s localhost:8080/v1/notes

curl -s -X DELETE localhost:8080/v1/notes/<id> \
  -H "Authorization: Bearer $GO_AGENTS_API_TOKEN"
```

## Adding a route

1. Bind it in `api.Router()`.
2. Document it in `api.operations` in `spec.go`. `SpecDrift()` fails the build
   if you skip this, so an undocumented route cannot ship.
3. `task openapi:write`, and commit the regenerated `docs/openapi.json`.
4. Update the table above.
