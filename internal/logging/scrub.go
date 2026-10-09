package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Keeping secrets out of logs by never passing them to a log call is safety by
// omission: it holds only as long as every future call site remembers. One
// added field — a header dump, a request body, a config struct — undoes it
// silently, and the failure is invisible until the logs are already written.
//
// Scrubbing at the WRITER makes it enforced instead. Every line the logger
// emits passes through here, including fields nobody has written yet.
//
// Redaction splices the ORIGINAL bytes rather than re-encoding a decoded map.
// Re-encoding alphabetised keys, replaced invalid UTF-8 with U+FFFD and
// collapsed duplicate keys, so any line that merely looked suspicious came out
// reordered — breaking byte-level greps on lines holding no secret at all.
// Splicing rewrites only the spans that actually change.

// Redacted replaces a sensitive value. It is deliberately not the empty
// string: a reader must be able to tell "this was redacted" from "this was
// absent", or a missing credential looks identical to a hidden one.
const Redacted = "[REDACTED]"

// sensitiveWords are the field-name fragments whose value is replaced.
//
// A key is split on separators and every CONTIGUOUS RUN of its words is
// tested, so composite names match without a substring rule that would fire on
// unrelated keys:
//
//	auth_token          -> auth | token | authtoken           -> match
//	secret_key          -> secret | key | secretkey           -> match
//	service_api_token   -> ... apitoken | serviceapitoken    -> match
//	author              -> author                             -> NO match
//
// Plain substring matching would redact "author" because it contains "auth".
// Whole-key matching — the previous behaviour — missed every composite above.
var sensitiveWords = map[string]struct{}{
	"authorization": {}, "auth": {}, "token": {}, "apitoken": {},
	"accesstoken": {}, "refreshtoken": {}, "idtoken": {}, "bearer": {},
	"password": {}, "passwd": {}, "secret": {}, "apikey": {}, "apisecret": {},
	"clientsecret": {}, "privatekey": {}, "cookie": {}, "setcookie": {},
	"sessionid": {}, "session": {}, "csrf": {}, "signature": {},
	"credential": {}, "credentials": {}, "ssn": {}, "creditcard": {},
	"cardnumber": {},
}

// keySplitter splits a field name into words on any separator style.
var keySplitter = regexp.MustCompile(`[_\-. ]+`)

// isSensitiveKey reports whether a field name's value must be redacted.
func isSensitiveKey(k string) bool {
	words := keySplitter.Split(strings.ToLower(k), -1)
	for i := range words {
		var joined strings.Builder
		for j := i; j < len(words); j++ {
			joined.WriteString(words[j])
			if _, ok := sensitiveWords[joined.String()]; ok {
				return true
			}
		}
	}
	return false
}

// valuePatterns redact secrets that appear INSIDE an otherwise innocuous
// value — a bearer token in a free-text message, for example. Kept narrow and
// bounded: a greedy pattern here would shred legitimate log content.
var valuePatterns = []*regexp.Regexp{
	// "Bearer <token>". The length floor is 16, not 8: at 8 this matched
	// ordinary prose — "Bearer authentication is required" lost its subject.
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-+/=]{16,}\b`),
	// PEM private key headers.
	regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	// Common provider key shapes.
	regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{8,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
}

// valueLiterals are the fixed fragments the patterns above key off. They feed
// the prefilter so a new pattern cannot silently bypass it.
var valueLiterals = []string{
	"bearer", "beginprivate", "begin ",
	"sk_", "pk_", "rk_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_",
}

// candidateMarkers is DERIVED, never hand-maintained. The previous hand-copied
// list was case-sensitive with two or three spellings per fragment, so
// CREDIT_CARD, SIGNATURE and Ssn slipped past the prefilter and were never
// examined — and the test meant to catch that re-derived its own copy of the
// key list, so it could not.
var candidateMarkers = buildMarkers()

func buildMarkers() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(sensitiveWords)+len(valueLiterals))
	add := func(s string) {
		s = separatorStripper.Replace(strings.ToLower(s))
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for w := range sensitiveWords {
		add(w)
	}
	for _, l := range valueLiterals {
		add(l)
	}
	return out
}

// separatorStripper removes key separators so a marker like "creditcard"
// matches a field spelled credit_card, credit-card or CreditCard.
var separatorStripper = strings.NewReplacer("_", "", "-", "", ".", "", " ", "")

// mayContainSecret reports whether a line is worth the full walk. It folds
// case and strips separators first, so capitalisation and naming style cannot
// route a secret past the fast path.
func mayContainSecret(line []byte) bool {
	folded := separatorStripper.Replace(strings.ToLower(string(line)))
	for _, m := range candidateMarkers {
		if strings.Contains(folded, m) {
			return true
		}
	}
	return false
}

// scrubValue redacts secret-shaped substrings inside a string value.
func scrubValue(s string) string {
	for _, re := range valuePatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}

// ScrubJSONLine redacts one serialized log line, preserving key order, number
// formatting and framing. A line that is not exactly one JSON value — a
// console-formatted line, a partial write, or a batch of two documents — still
// gets the value patterns applied, so the fallback is never a silent
// passthrough and never a truncation.
func ScrubJSONLine(line []byte) []byte {
	// Fast path: nothing that could match, so hand back the caller's bytes
	// untouched rather than walking them.
	if !mayContainSecret(line) {
		return line
	}

	body := bytes.TrimRight(line, "\n")
	trailing := line[len(body):]

	out, ok := spliceJSON(body)
	if !ok {
		return []byte(scrubValue(string(line)))
	}
	return append(out, trailing...)
}

// spliceJSON walks the document and rewrites only the byte spans that change.
// It reports false when the input is not exactly one JSON value, so the caller
// falls back rather than emitting a truncated document.
func spliceJSON(src []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(src)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}

	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()

	var out bytes.Buffer
	copied := 0

	// expectKey tracks, per nesting level, whether the next string token is a
	// field name rather than a value. redactDepth is the depth at which a
	// sensitive key was seen; every scalar at or below it is redacted, so a
	// sensitive key holding an object still has its leaves scrubbed.
	var isObject, expectKey []bool
	redactDepth := -1
	redactNext := false

	redactSpan := func(start, end int) {
		out.Write(src[copied:start])
		out.WriteString(`"` + Redacted + `"`)
		copied = end
	}

	for {
		prevEnd := int(dec.InputOffset())
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		start := firstTokenByte(src, prevEnd)
		end := int(dec.InputOffset())

		depth := len(isObject)
		inObject := depth > 0 && isObject[depth-1]
		atKey := inObject && expectKey[depth-1]

		if d, isDelim := tok.(json.Delim); isDelim {
			switch d {
			case '{', '[':
				if redactNext {
					// A sensitive key whose value is a container: redact every
					// leaf inside rather than flattening the structure.
					redactDepth = depth
					redactNext = false
				}
				if inObject {
					expectKey[depth-1] = true
				}
				isObject = append(isObject, d == '{')
				expectKey = append(expectKey, d == '{')
			case '}', ']':
				isObject = isObject[:len(isObject)-1]
				expectKey = expectKey[:len(expectKey)-1]
				if redactDepth >= len(isObject) {
					redactDepth = -1
				}
			}
			continue
		}

		if atKey {
			expectKey[depth-1] = false
			if s, ok := tok.(string); ok && isSensitiveKey(s) {
				redactNext = true
			}
			continue
		}
		if inObject {
			expectKey[depth-1] = true
		}

		if redactNext || redactDepth >= 0 {
			redactSpan(start, end)
			redactNext = false
			continue
		}
		if s, ok := tok.(string); ok {
			if scrubbed := scrubValue(s); scrubbed != s {
				quoted, err := json.Marshal(scrubbed)
				if err != nil {
					return nil, false
				}
				out.Write(src[copied:start])
				out.Write(quoted)
				copied = end
			}
		}
	}

	// Anything after the first value means the write held a batch, which an
	// earlier version silently discarded along with any secret in it.
	if rest := bytes.TrimSpace(src[dec.InputOffset():]); len(rest) > 0 {
		return nil, false
	}

	out.Write(src[copied:])
	return out.Bytes(), true
}

// firstTokenByte returns the index of the next byte that begins a token,
// skipping the whitespace and separators the decoder does not report.
func firstTokenByte(src []byte, from int) int {
	for i := from; i < len(src); i++ {
		switch src[i] {
		case ' ', '\t', '\r', '\n', ',', ':':
		default:
			return i
		}
	}
	return len(src)
}

// scrubWriter redacts every line written through it.
type scrubWriter struct{ w io.Writer }

// NewScrubWriter wraps w so every log line is scrubbed before it is written.
// Wrap at construction, not at call sites — the point is that a future field
// cannot bypass it.
func NewScrubWriter(w io.Writer) io.Writer { return &scrubWriter{w: w} }

func (s *scrubWriter) Write(p []byte) (int, error) {
	if _, err := s.w.Write(ScrubJSONLine(p)); err != nil {
		return 0, fmt.Errorf("write scrubbed log line: %w", err)
	}
	// Report the caller's length: zerolog treats a short write as an error,
	// and the redacted line is legitimately a different size.
	return len(p), nil
}
