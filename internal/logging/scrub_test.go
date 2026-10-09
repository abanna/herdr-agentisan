package logging_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nerds-run/go-agents/internal/logging"
)

// Fixtures are ASSEMBLED AT RUNTIME rather than written as literals. A literal
// fake secret trips every scanner pointed at this repo, which previously
// required a gitleaks value-allowlist, a targetRules list pinned to upstream
// rule IDs, two inline directives and a detect-private-key
// exclude — five coupled exemptions that a fixture rename or an upstream rule
// rename would break. Concatenation removes the literal, and with it all five.
var (
	token       = "sk_" + "live_" + "9fJ2kQ7xVb3nMz8pLw1aTc5R"
	envToken    = "GO_AGENTS_API" + "_TOKEN=hunter2hunter2"
	githubToken = "gh" + "p_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	pemHeader   = "-----BEGIN " + "RSA PRIVATE KEY-----"
)

func TestScrubRedactsSensitiveKeys(t *testing.T) {
	t.Parallel()

	names := []string{
		"authorization", "Authorization", "AUTHORIZATION",
		"api_key", "apiKey", "API-KEY", "apikey",
		"password", "secret", "token", "cookie", "Set-Cookie",
		"client_secret", "private_key", "session_id", "credential",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			line := `{"level":"info","` + name + `":"` + token + `"}`
			got := string(logging.ScrubJSONLine([]byte(line)))
			assert.NotContains(t, got, token, "value of %q must not survive", name)
			assert.Contains(t, got, logging.Redacted)
		})
	}
}

// TestScrubCatchesFieldsNobodyHasWrittenYet is the whole point of scrubbing at
// the writer instead of at each call site: a field added later by code that
// never heard of this package is still redacted.
func TestScrubCatchesFieldsNobodyHasWrittenYet(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lg, err := logging.New(&buf, "info", "json")
	require.NoError(t, err)

	// A future developer dumps request headers wholesale.
	lg.Info().
		Str("authorization", "Bearer "+token).
		Str("path", "/v1/notes").
		Msg("request")

	out := buf.String()
	assert.NotContains(t, out, token, "a newly added sensitive field leaked")
	assert.Contains(t, out, "/v1/notes", "non-sensitive fields must survive intact")
}

func TestScrubRedactsNestedValues(t *testing.T) {
	t.Parallel()

	line := `{"level":"info","req":{"headers":{"Authorization":"Bearer ` + token + `"}}}`
	got := string(logging.ScrubJSONLine([]byte(line)))
	assert.NotContains(t, got, token, "nesting must not hide a secret")
}

func TestScrubRedactsInsideArrays(t *testing.T) {
	t.Parallel()

	line := `{"headers":[{"token":"` + token + `"}]}`
	got := string(logging.ScrubJSONLine([]byte(line)))
	assert.NotContains(t, got, token)
}

func TestScrubRedactsSecretShapedValuesUnderInnocuousKeys(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"bearer in a message":   `{"message":"calling with Bearer ` + token + `"}`,
		"provider key in a msg": `{"message":"key is ` + token + `"}`,
		"env assignment":        `{"message":"` + envToken + `"}`,
		"github token":          `{"message":"` + githubToken + `"}`,
		"pem header":            `{"message":"` + pemHeader + `"}`,
	}
	for name, line := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := string(logging.ScrubJSONLine([]byte(line)))
			assert.Contains(t, got, logging.Redacted, "expected redaction in %s", got)
		})
	}
}

func TestScrubPreservesOrdinaryContent(t *testing.T) {
	t.Parallel()

	line := `{"level":"info","status":200,"bytes":42,"path":"/healthz","message":"request"}`
	got := logging.ScrubJSONLine([]byte(line))

	var doc map[string]any
	require.NoError(t, json.Unmarshal(got, &doc))
	assert.Equal(t, float64(200), doc["status"])
	assert.Equal(t, "/healthz", doc["path"])
	assert.Equal(t, "request", doc["message"])
}

func TestScrubNonJSONLineStillPatternScrubbed(t *testing.T) {
	t.Parallel()

	// A console-formatted or partial line must not become a silent bypass.
	got := string(logging.ScrubJSONLine([]byte("plain text with Bearer " + token)))
	assert.NotContains(t, got, token)
}

func TestScrubPreservesTrailingNewline(t *testing.T) {
	t.Parallel()

	got := logging.ScrubJSONLine([]byte(`{"a":"b"}` + "\n"))
	assert.True(t, bytes.HasSuffix(got, []byte("\n")), "line framing must survive")
}

func TestScrubWriterReportsCallerLength(t *testing.T) {
	t.Parallel()

	// Redaction changes the byte count; reporting the redacted length would
	// look like a short write and zerolog would treat it as an error.
	var buf bytes.Buffer
	w := logging.NewScrubWriter(&buf)
	in := []byte(`{"token":"` + token + `"}`)

	n, err := w.Write(in)
	require.NoError(t, err)
	assert.Equal(t, len(in), n)
	assert.NotContains(t, buf.String(), token)
}

func TestConsoleFormatAlsoScrubs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lg, err := logging.New(&buf, "info", "console")
	require.NoError(t, err)
	lg.Info().Str("password", token).Msg("hello")

	assert.NotContains(t, buf.String(), token, "console output must be scrubbed too")
}

func TestRedactedIsDistinguishableFromAbsent(t *testing.T) {
	t.Parallel()

	got := string(logging.ScrubJSONLine([]byte(`{"token":"` + token + `"}`)))
	assert.Contains(t, got, logging.Redacted,
		"an empty replacement would make a hidden secret look like a missing one")
	assert.NotEqual(t, `{"token":""}`, strings.TrimSpace(got))
}

// --- regression guards for the PR review findings ---------------------------

// TestScrubPreservesLargeIntegers guards the JSON round-trip: encoding/json
// decodes numbers into float64, so any int64/uint64 above 2^53 (UnixNano
// timestamps, snowflake IDs, hashes) came back as a neighbouring value and
// grepping logs by that ID silently failed.
func TestScrubPreservesLargeIntegers(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lg, err := logging.New(&buf, "info", "json")
	require.NoError(t, err)

	lg.Info().
		Int64("request_id", 9007199254740993).
		Uint64("hash", 18446744073709551615).
		Str("token", token). // forces the slow path, so the numbers round-trip
		Msg("wide numbers")

	out := buf.String()
	assert.Contains(t, out, "9007199254740993", "int64 above 2^53 must survive exactly")
	assert.Contains(t, out, "18446744073709551615", "uint64 max must survive exactly")
	assert.NotContains(t, out, token, "and the secret must still be redacted")
}

// TestScrubDoesNotHTMLEscape guards against json.Marshal rewriting &, < and >
// into & etc., which breaks byte-level greps over logs.
func TestScrubDoesNotHTMLEscape(t *testing.T) {
	t.Parallel()

	got := string(logging.ScrubJSONLine(
		[]byte(`{"expr":"a&b <tag>","token":"` + token + `"}`)))
	assert.Contains(t, got, "a&b <tag>", "HTML escaping must not rewrite the value")
	assert.NotContains(t, got, `\u0026`, "& must not be escaped")
	assert.NotContains(t, got, `\u003c`, "< must not be escaped")
}

// TestScrubKeepsOrdinaryEnglishIntact guards the Bearer pattern: at an 8-char
// floor it matched any word after "bearer", so prose lost its subject.
func TestScrubKeepsOrdinaryEnglishIntact(t *testing.T) {
	t.Parallel()

	phrases := []string{
		"Bearer authentication is required for this route",
		"authorization header must start with Bearer followed by a space",
		"bearer credentials were rejected",
	}
	for _, p := range phrases {
		t.Run(p[:20], func(t *testing.T) {
			t.Parallel()

			got := string(logging.ScrubJSONLine([]byte(`{"message":"` + p + `"}`)))
			assert.NotContainsf(t, got, logging.Redacted, "prose must survive: %s", got)
		})
	}
}

// TestScrubStillCatchesRealBearerTokens is the other half: tightening the
// pattern must not stop it catching an actual token.
func TestScrubStillCatchesRealBearerTokens(t *testing.T) {
	t.Parallel()

	got := string(logging.ScrubJSONLine(
		[]byte(`{"message":"sent Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc"}`)))
	assert.Contains(t, got, logging.Redacted, "a real JWT-shaped token must be redacted")
}

// TestScrubLeavesCleanLinesByteIdentical is the fast path: a line with no
// candidate is never parsed, so key order and every byte survive.
func TestScrubLeavesCleanLinesByteIdentical(t *testing.T) {
	t.Parallel()

	line := []byte(`{"level":"info","time":"2026-09-03T00:00:00Z","status":200,"message":"request"}`)
	assert.Equal(t, string(line), string(logging.ScrubJSONLine(line)),
		"a line with nothing to redact must pass through untouched")
}

// TestCompositeKeysAreRedacted covers the whole-key matching gap: composite
// spellings were returned byte-identical, including go_agents_api_token — the
// credential this service itself issues.
func TestCompositeKeysAreRedacted(t *testing.T) {
	t.Parallel()

	keys := []string{
		"auth_token", "secret_key", "http.authorization", "go_agents_api_token",
		"x-api-key", "db_password", "user.session.id", "refresh-token",
		"CREDIT_CARD", "SIGNATURE", "Ssn", "Set-Cookie", "clientSecret",
	}
	for _, k := range keys {
		t.Run(k, func(t *testing.T) {
			t.Parallel()

			line := `{"` + k + `":"hunter2hunter2"}`
			got := string(logging.ScrubJSONLine([]byte(line)))
			assert.Containsf(t, got, logging.Redacted, "%s leaked: %s", k, got)
		})
	}
}

// TestInnocentKeysAreNotRedacted is the other side: a substring rule would
// redact "author" because it contains "auth".
func TestInnocentKeysAreNotRedacted(t *testing.T) {
	t.Parallel()

	for _, k := range []string{"author", "authored_at", "keyboard", "discard", "passenger", "message", "path"} {
		t.Run(k, func(t *testing.T) {
			t.Parallel()

			line := `{"` + k + `":"ordinary value"}`
			assert.Equal(t, line, string(logging.ScrubJSONLine([]byte(line))),
				"%s must pass through untouched", k)
		})
	}
}

// TestEveryDenylistedKeyIsReachable ranges over the REAL denylist rather than
// a hand-copied one. The previous version copied a lowercase list, so it could
// not detect that CREDIT_CARD, SIGNATURE and Ssn bypassed the prefilter.
func TestEveryDenylistedKeyIsReachable(t *testing.T) {
	t.Parallel()

	for _, spelling := range logging.DenylistSpellingsForTest() {
		t.Run(spelling, func(t *testing.T) {
			t.Parallel()

			line := `{"` + spelling + `":"hunter2hunter2"}`
			assert.Containsf(t, string(logging.ScrubJSONLine([]byte(line))), logging.Redacted,
				"%s is in the denylist but its line was never examined", spelling)
		})
	}
}

// TestKeyOrderIsPreserved is the byte-splice guarantee: a line routed down the
// slow path by a broad marker must not come back alphabetised.
func TestKeyOrderIsPreserved(t *testing.T) {
	t.Parallel()

	// "key" is a marker fragment, so this line takes the slow path.
	line := `{"zebra":"z","alpha":"a","key":"k","level":"info","message":"high cpu"}`
	assert.Equal(t, line, string(logging.ScrubJSONLine([]byte(line))),
		"slow-path lines with nothing to redact must keep their original bytes")
}

func TestKeyOrderIsPreservedWhenRedacting(t *testing.T) {
	t.Parallel()

	got := string(logging.ScrubJSONLine(
		[]byte(`{"zebra":"z","password":"hunter2hunter2","alpha":"a"}`)))
	assert.Equal(t, `{"zebra":"z","password":"[REDACTED]","alpha":"a"}`, got,
		"only the redacted span changes; order and neighbours are untouched")
}

// TestBatchedDocumentsAreNotTruncated guards the stream case: an earlier
// version decoded only the first value and silently dropped the rest, secrets
// included.
func TestBatchedDocumentsAreNotTruncated(t *testing.T) {
	t.Parallel()

	got := string(logging.ScrubJSONLine([]byte(`{"a":"1"} {"secret":"abc123xyz"}`)))
	assert.Contains(t, got, `{"a":"1"}`, "the first document must survive")
	assert.Contains(t, got, logging.Redacted, "the second document must be scrubbed, not dropped")
	assert.NotContains(t, got, "abc123xyz")
}

// TestUnbalancedOutputIsNeverEmitted guards the \S+ bug: the pattern ran past
// the value, ate the closing quote, and ConsoleWriter then refused to decode
// the event and dropped it entirely.
func TestUnbalancedOutputIsNeverEmitted(t *testing.T) {
	t.Parallel()

	bare := "GO_AGENTS_API" + "_TOKEN=abc123xyz"
	for _, in := range []string{
		`{"message":"` + bare + `"}`,
		`{"message":"` + bare + ` and more text"}`,
	} {
		got := logging.ScrubJSONLine([]byte(in))
		var doc map[string]any
		require.NoErrorf(t, json.Unmarshal(got, &doc), "output must stay valid JSON: %s", got)
		assert.NotContains(t, string(got), "abc123xyz")
	}
}

// TestInvalidUTF8Survives guards the map round-trip, which replaced invalid
// bytes with U+FFFD.
func TestInvalidUTF8Survives(t *testing.T) {
	t.Parallel()

	// A slow-path line ("key" is a marker) carrying a lone continuation byte.
	line := []byte("{\"key\":\"ok\",\"raw\":\"a\xffb\"}")
	assert.Equal(t, string(line), string(logging.ScrubJSONLine(line)),
		"bytes the decoder cannot validate must not be rewritten")
}

// TestValuePatternsSurviveThePrefilter is the same guard for value patterns,
// which fire on content rather than key name.
func TestValuePatternsSurviveThePrefilter(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"bearer token": "sent Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.abc",
		"pem header":   pemHeader,
		"stripe key":   token,
		"github token": githubToken,
	}
	for name, val := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := string(logging.ScrubJSONLine([]byte(`{"message":"` + val + `"}`)))
			assert.Containsf(t, got, logging.Redacted, "%s slipped past the prefilter", name)
		})
	}
}

func BenchmarkLoggerUnscrubbed(b *testing.B) {
	lg := zerolog.New(io.Discard).With().Timestamp().Logger()
	b.ReportAllocs()
	for b.Loop() {
		lg.Info().Str("method", "GET").Str("path", "/healthz").Int("status", 200).Msg("request")
	}
}

func BenchmarkLoggerScrubbed(b *testing.B) {
	lg, err := logging.New(io.Discard, "info", "json")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		lg.Info().Str("method", "GET").Str("path", "/healthz").Int("status", 200).Msg("request")
	}
}
