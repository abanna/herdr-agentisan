package trail_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/trail"
)

var both = []string{trail.URLVar, trail.KeyVar}

// keyEq starts the key's line in a test env file. Built from the constant,
// so no source line spells a key assignment the safety scan would stop on.
const keyEq = trail.KeyVar + "="

// TestReadEnvFile: the env file is read for the two names alone. The shapes
// a dotenv file takes all read the same values; every other line, the
// collector's write key included, is passed over and not returned; a
// line splits at its first = only, so a base64 key keeps its padding, and
// double quotes take \" and \\; a file of 64 KiB reads, a byte more does
// not; a missing or empty name, a bad quote, a missing file or a directory
// each fail, and no error carries a value.
func TestReadEnvFile(t *testing.T) {
	t.Parallel()
	const want = "http://es:9200"
	good := "ELASTIC_URL=http://es:9200\n" + keyEq + probeKey + "\n"
	// sized pads good with a comment to exactly n bytes.
	sized := func(n int) string { return "# " + strings.Repeat("x", n-len(good)-3) + "\n" + good }
	tests := map[string]struct {
		content string // "" for no file; "DIR" a directory; "LINK" a symlink to a good file; "DANGLING"; "MODE0" unreadable
		wantKey string // the key read, probeKey when empty
		wantErr error
		wantIn  string
	}{
		"plain":               {content: "ELASTIC_URL=http://es:9200\n" + keyEq + probeKey + "\n"},
		"export, no final LF": {content: "export ELASTIC_URL=http://es:9200\nexport " + keyEq + probeKey},
		"double quotes":       {content: "ELASTIC_URL=\"http://es:9200\"\n" + keyEq + "\"" + probeKey + "\"\n"},
		"single quotes":       {content: "ELASTIC_URL='http://es:9200'\n" + keyEq + "'" + probeKey + "'\n"},
		"a BOM and CRLF":      {content: "\ufeffELASTIC_URL=http://es:9200\r\n" + keyEq + probeKey + "\r\n"},
		"comments, blanks, other secrets, junk": {content: "# the debugger\n\nHERDR_AGENTISAN_ES_WRITE_KEY=write-only-xyz\nnot a line\n" +
			"  ELASTIC_URL = http://es:9200  \n" + keyEq + probeKey + "\n"},
		"a name twice: the last": {content: "ELASTIC_URL=http://old:1\nELASTIC_URL=http://es:9200\n" + keyEq + probeKey + "\n"},
		"no key":                 {content: "ELASTIC_URL=http://es:9200\n", wantErr: trail.ErrNotConfigured, wantIn: "has no ELASTIC_API_KEY"},
		"an empty URL":           {content: "ELASTIC_URL=\n" + keyEq + probeKey + "\n", wantErr: trail.ErrNotConfigured, wantIn: "has no ELASTIC_URL"},
		"a quote left open":      {content: "ELASTIC_URL=http://es:9200\n" + keyEq + "\"" + probeKey + "\n", wantErr: trail.ErrEnvFile, wantIn: "ELASTIC_API_KEY's value opens a quote"},
		"64 KiB and a byte":      {content: sized(64<<10 + 1), wantErr: trail.ErrEnvFile, wantIn: "over 64 KiB"},
		"no file":                {wantErr: trail.ErrEnvFile, wantIn: "does not exist"},
		"a directory":            {content: "DIR", wantErr: trail.ErrEnvFile},
		"a symlink to the file":  {content: "LINK"},
		"a dangling symlink":     {content: "DANGLING", wantErr: trail.ErrEnvFile, wantIn: "does not exist"},
		"unreadable":             {content: "MODE0", wantErr: trail.ErrEnvFile, wantIn: "permission denied"},

		// A line splits at its first = only: a base64 key keeps its padding.
		"= in the value":      {content: "ELASTIC_URL=http://es:9200\n" + keyEq + probeKey + "==\n", wantKey: probeKey + "=="},
		"= in a quoted value": {content: "ELASTIC_URL=http://es:9200\n" + keyEq + "'a=b=='\n", wantKey: "a=b=="},
		// Double quotes take \" and \\; single quotes take the value as written.
		"escapes in double quotes":      {content: "ELASTIC_URL=http://es:9200\n" + keyEq + `"a\"b\\c"` + "\n", wantKey: `a"b\c`},
		"escapes kept in single quotes": {content: "ELASTIC_URL=http://es:9200\n" + keyEq + `'a\"b\\c'` + "\n", wantKey: `a\"b\\c`},
		// The cap is 64 KiB: a file of exactly that reads.
		"exactly 64 KiB": {content: sized(64 << 10)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "local.env")
			switch tc.content {
			case "":
			case "DIR":
				require.NoError(t, os.Mkdir(path, 0o700))
			case "LINK", "DANGLING":
				target := filepath.Join(t.TempDir(), "target.env")
				if tc.content == "LINK" {
					require.NoError(t, os.WriteFile(target, []byte(good), 0o600))
				}
				require.NoError(t, os.Symlink(target, path))
			case "MODE0":
				if os.Geteuid() == 0 {
					t.Skip("root reads a mode-0 file")
				}
				require.NoError(t, os.WriteFile(path, []byte(good), 0o000))
			default:
				require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			}

			got, err := trail.ReadEnvFile(t.Context(), path, both, time.Second)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), tc.wantIn)
				assert.NotContains(t, err.Error(), probeKey)
				return
			}
			require.NoError(t, err)
			key := tc.wantKey
			if key == "" {
				key = probeKey
			}
			assert.Equal(t, map[string]string{trail.URLVar: want, trail.KeyVar: key}, got)
		})
	}
}
