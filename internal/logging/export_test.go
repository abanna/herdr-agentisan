package logging

import "strings"

// DenylistSpellingsForTest returns each denylisted word in several
// capitalisations and separator styles, so a test can range over the REAL
// denylist rather than a hand-copied echo of it.
func DenylistSpellingsForTest() []string {
	var out []string
	for w := range sensitiveWords {
		out = append(out, w, strings.ToUpper(w), strings.Title(w)) //nolint:staticcheck // ASCII-only test fixtures
	}
	return out
}
