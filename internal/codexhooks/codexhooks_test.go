package codexhooks_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/codexhooks"
)

// ours is the group Install writes for an event, as raw JSON.
func ours(event, command string, extra string) string {
	matcher := ""
	if event == "PostToolUse" {
		matcher = `"matcher":"*",`
	}
	return `{` + matcher + `"hooks":[{"type":"command","command":"` + command + `","timeout":10,"async":true` + extra + `}]}`
}

const (
	cmd     = "herdr-agentisan report codex"
	userPre = `{"matcher":"Bash","hooks":[{"type":"command","command":"lint"}]}`
	userA   = `{"matcher":"Edit","hooks":[{"type":"command","command":"fmt"}]}`
	userB   = `{"hooks":[{"type":"command","command":"notify","async":true}]}`
)

// installed is a whole hooks.json with all three of ours.
var installed = `{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + ours("Stop", cmd, "") + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `]}}`

// decode parses JSON for a comparison that ignores layout and key order.
func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(s), &v), s)
	return v
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a test's own temp file
	require.NoError(t, err)
	return string(b)
}

func states(r codexhooks.Result) map[string]codexhooks.State {
	out := map[string]codexhooks.State{}
	for _, h := range r.Hooks {
		out[h.Event] = h.State
	}
	return out
}

func events(r codexhooks.Result) []string {
	var out []string
	for _, h := range r.Hooks {
		out = append(out, h.Event)
	}
	return out
}

// TestHooksMatchTheSample pins the hooks the command writes to the sample
// and README: a drift between them would make an install differ from what
// the docs say, and an edited hook loses Codex's trust (A22).
func TestHooksMatchTheSample(t *testing.T) {
	t.Parallel()
	sample, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "codex-hooks.example.toml"))
	require.NoError(t, err)
	var cfg struct {
		Hooks map[string][]codexhooks.Group `toml:"hooks"`
	}
	dec := toml.NewDecoder(bytes.NewReader(sample))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&cfg))

	want := map[string][]codexhooks.Group{}
	for event, g := range codexhooks.Hooks() {
		want[event] = []codexhooks.Group{g}
	}
	assert.Equal(t, want, cfg.Hooks)
	assert.Len(t, cfg.Hooks, 3, "PostToolUse, Stop and PostCompact; never SessionEnd (A27)")
}

func TestHooksIsACopy(t *testing.T) {
	t.Parallel()
	h := codexhooks.Hooks()
	g := h["Stop"]
	g.Hooks[0].Command = "changed"
	h["Stop"] = g
	assert.Equal(t, cmd, codexhooks.Hooks()["Stop"].Hooks[0].Command)
}

func TestInstall(t *testing.T) {
	t.Parallel()
	added := map[string]codexhooks.State{"PostToolUse": codexhooks.Added, "Stop": codexhooks.Added, "PostCompact": codexhooks.Added}
	tests := map[string]struct {
		start   *string // nil: no hooks.json
		want    string
		states  map[string]codexhooks.State
		changed bool
	}{
		"no file":                {want: installed, states: added, changed: true},
		"an empty one":           {start: new(""), want: installed, states: added, changed: true},
		"whitespace":             {start: new(" \n\t"), want: installed, states: added, changed: true},
		"{}":                     {start: new("{}"), want: installed, states: added, changed: true},
		"hooks {}":               {start: new(`{"hooks":{}}`), want: installed, states: added, changed: true},
		"null events":            {start: new(`{"hooks":{"Stop":null}}`), want: installed, states: added, changed: true},
		"CRLF, no final newline": {start: new("{\r\n\"hooks\": {}\r\n}"), want: installed, states: added, changed: true},
		"a description and other events kept": {
			start:   new(`{"description":"mine","hooks":{"PreToolUse":[` + userPre + `]}}`),
			want:    `{"description":"mine","hooks":{"PreToolUse":[` + userPre + `],"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[` + ours("Stop", cmd, "") + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `]}}`,
			states:  added,
			changed: true,
		},
		"ours go after the user's own": {
			start:   new(`{"hooks":{"PostToolUse":[` + userA + `],"Stop":[` + userB + `]}}`),
			want:    `{"hooks":{"PostToolUse":[` + userA + `,` + ours("PostToolUse", cmd, "") + `],"Stop":[` + userB + `,` + ours("Stop", cmd, "") + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `]}}`,
			states:  added,
			changed: true,
		},
		"already installed": {
			start:  new(installed),
			want:   installed,
			states: map[string]codexhooks.State{"PostToolUse": codexhooks.Unchanged, "Stop": codexhooks.Unchanged, "PostCompact": codexhooks.Unchanged},
		},
		"another path and a redirect are still ours": {
			start: new(`{"hooks":{"PostToolUse":[` + ours("PostToolUse", "/home/u/go/bin/herdr-agentisan report codex", "") + `],"Stop":[` + ours("Stop", "herdr-agentisan  report codex >/dev/null 2>&1", "") + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `]}}`),
			want:  `{"hooks":{"PostToolUse":[` + ours("PostToolUse", "/home/u/go/bin/herdr-agentisan report codex", "") + `],"Stop":[` + ours("Stop", "herdr-agentisan  report codex >/dev/null 2>&1", "") + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `]}}`,
			states: map[string]codexhooks.State{
				"PostToolUse": codexhooks.Unchanged, "Stop": codexhooks.Unchanged, "PostCompact": codexhooks.Unchanged,
			},
		},
		"an old one is updated in place, its command kept": {
			start: new(`{"hooks":{"PostToolUse":[` + userA + `,{"matcher":"Bash","hooks":[{"type":"command","command":"/opt/herdr-agentisan report codex","timeout":5}]},` + userB + `],"Stop":[` + ours("Stop", cmd, "") + `]}}`),
			want:  `{"hooks":{"PostToolUse":[` + userA + `,` + ours("PostToolUse", "/opt/herdr-agentisan report codex", "") + `,` + userB + `],"Stop":[` + ours("Stop", cmd, "") + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `]}}`,
			states: map[string]codexhooks.State{
				"PostToolUse": codexhooks.Updated, "Stop": codexhooks.Unchanged, "PostCompact": codexhooks.Added,
			},
			changed: true,
		},
		"a matcher on Stop, a field of its own, and not async are updated": {
			start: new(`{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, `,"statusMessage":"ctx"`) + `],"Stop":[{"matcher":"*","hooks":[{"type":"command","command":"herdr-agentisan report codex","timeout":10,"async":true}]}],"PostCompact":[{"hooks":[{"type":"command","command":"herdr-agentisan report codex","timeout":10}]}]}}`),
			want:  installed,
			states: map[string]codexhooks.State{
				"PostToolUse": codexhooks.Updated, "Stop": codexhooks.Updated, "PostCompact": codexhooks.Updated,
			},
			changed: true,
		},
		"look-alikes are not ours": {
			start:   new(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"herdr-agentisan report stage"}]},{"hooks":[{"type":"command","command":"my-herdr-agentisan report codex"}]},{"hooks":[{"type":"command","command":"herdr-agentisan report"}]},{"hooks":[{"type":"mcp_tool","server":"herdr-agentisan","tool":"report codex"}]},{"hooks":[]}]}}`),
			want:    `{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[{"hooks":[{"type":"command","command":"herdr-agentisan report stage"}]},{"hooks":[{"type":"command","command":"my-herdr-agentisan report codex"}]},{"hooks":[{"type":"command","command":"herdr-agentisan report"}]},{"hooks":[{"type":"mcp_tool","server":"herdr-agentisan","tool":"report codex"}]},{"hooks":[]},` + ours("Stop", cmd, "") + `],"PostCompact":[` + ours("PostCompact", cmd, "") + `]}}`,
			states:  added,
			changed: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			path := filepath.Join(home, codexhooks.FileName)
			if tc.start != nil {
				write(t, path, *tc.start, 0o600)
			}

			res, err := codexhooks.Install(home)
			require.NoError(t, err)
			assert.Equal(t, path, res.Path)
			assert.Equal(t, tc.changed, res.Changed)
			assert.Equal(t, tc.states, states(res))
			assert.Equal(t, []string{"PostToolUse", "Stop", "PostCompact"}, events(res))
			for _, h := range res.Hooks {
				assert.Equal(t, path, h.Source, h.Event)
			}
			got := read(t, path)
			assert.Equal(t, decode(t, tc.want), decode(t, got))
			if !tc.changed {
				assert.Equal(t, *tc.start, got, "nothing changed, nothing written")
			}
			assert.True(t, res.Installed())

			again, err := codexhooks.Install(home)
			require.NoError(t, err)
			assert.False(t, again.Changed, "a second install changes nothing")
			assert.Equal(t, got, read(t, path))
		})
	}
}

// TestInstallKeepsTheGroupsAsWritten checks a rewrite keeps the user's own
// groups, keys Codex ignores included, and changes none of their values.
func TestInstallKeepsTheGroupsAsWritten(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, codexhooks.FileName)
	odd := `{"matcher":"Bash","hooks":[{"type":"command","command":"lint \"a\u00e9\"","timeout":1e1,"x-note":{"k":[1,2]}}],"x-mine":true}`
	write(t, path, `{"hooks":{"PostToolUse":[`+odd+`],"Mystery":[1,"two"]}}`, 0o600)

	_, err := codexhooks.Install(home)
	require.NoError(t, err)

	var got struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal([]byte(read(t, path)), &got))
	assert.Equal(t, decode(t, odd), decode(t, string(got.Hooks["PostToolUse"][0])))
	assert.Equal(t, decode(t, `[1,"two"]`), decode(t, `[`+string(got.Hooks["Mystery"][0])+`,`+string(got.Hooks["Mystery"][1])+`]`))
}

func TestInstallRefuses(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		start string
		want  error
	}{
		"not JSON":                   {start: `{`, want: codexhooks.ErrInvalid},
		"trailing data":              {start: `{} {}`, want: codexhooks.ErrInvalid},
		"an array":                   {start: `[]`, want: codexhooks.ErrInvalid},
		"null":                       {start: `null`, want: codexhooks.ErrInvalid},
		"a string":                   {start: `"x"`, want: codexhooks.ErrInvalid},
		"a key Codex refuses":        {start: `{"hooks":{},"version":1}`, want: codexhooks.ErrInvalid},
		"hooks not an object":        {start: `{"hooks":[]}`, want: codexhooks.ErrInvalid},
		"hooks null":                 {start: `{"hooks":null}`, want: codexhooks.ErrInvalid},
		"an event not an array":      {start: `{"hooks":{"Stop":{}}}`, want: codexhooks.ErrInvalid},
		"a group not an object":      {start: `{"hooks":{"Stop":[1]}}`, want: codexhooks.ErrInvalid},
		"a group's hooks not a list": {start: `{"hooks":{"Stop":[{"hooks":{}}]}}`, want: codexhooks.ErrInvalid},
		"a handler not an object":    {start: `{"hooks":{"Stop":[{"hooks":[1]}]}}`, want: codexhooks.ErrInvalid},
		"a command not a string":     {start: `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":7}]}]}}`, want: codexhooks.ErrInvalid},
		"a matcher not a string":     {start: `{"hooks":{"Stop":[{"matcher":1,"hooks":[]}]}}`, want: codexhooks.ErrInvalid},
		"invalid UTF-8":              {start: "{\"description\":\"\xff\"}", want: codexhooks.ErrInvalid},
		"a byte order mark":          {start: "\xef\xbb\xbf{}", want: codexhooks.ErrInvalid},
		"a NUL":                      {start: "{\"description\":\"a\x00\"}", want: codexhooks.ErrInvalid},
		"ours beside another hook": {
			start: `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"notify"},{"type":"command","command":"herdr-agentisan report codex"}]}]}}`,
			want:  codexhooks.ErrAmbiguous,
		},
		"ours twice in one event": {
			start: `{"hooks":{"Stop":[` + ours("Stop", cmd, "") + `,` + userB + `,` + ours("Stop", "/x/herdr-agentisan report codex", "") + `]}}`,
			want:  codexhooks.ErrAmbiguous,
		},
		// Valid JSON once cut at the cap, so only the cap refuses it.
		"too large": {start: "{}" + strings.Repeat(" ", codexhooks.MaxFileSize), want: codexhooks.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			path := filepath.Join(home, codexhooks.FileName)
			write(t, path, tc.start, 0o600)

			_, err := codexhooks.Install(home)
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), path, "the error names the file")
			assert.Equal(t, tc.start, read(t, path), "a refused file is left as it was")

			_, err = codexhooks.Check(home)
			require.ErrorIs(t, err, tc.want, "check refuses what install refuses")
			assertOnly(t, home, codexhooks.FileName)
		})
	}
}

// assertOnly checks dir holds exactly names: no temp file is left behind.
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	assert.ElementsMatch(t, names, got)
}

func TestInstallRefusesWhatIsNotAFile(t *testing.T) {
	t.Parallel()
	t.Run("a directory", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(home, codexhooks.FileName), 0o700))
		_, err := codexhooks.Install(home)
		require.ErrorIs(t, err, codexhooks.ErrInvalid)
		_, err = codexhooks.Check(home)
		require.ErrorIs(t, err, codexhooks.ErrInvalid)
	})
	t.Run("a FIFO is never opened", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		require.NoError(t, syscall.Mkfifo(filepath.Join(home, codexhooks.FileName), 0o600))
		_, err := codexhooks.Install(home)
		require.ErrorIs(t, err, codexhooks.ErrInvalid)
	})
	t.Run("a home that is a file", func(t *testing.T) {
		t.Parallel()
		home := filepath.Join(t.TempDir(), "codex")
		write(t, home, "", 0o600)
		_, err := codexhooks.Install(home)
		require.Error(t, err)
	})
}

// TestInstallRefusesWhatARenameWouldBreak covers the two files herdr's own
// Codex installer also refuses to replace: one with other hard links, which
// a rename would split from them, and one the user cannot write, whose
// directory would otherwise let a rename replace it all the same.
func TestInstallRefusesWhatARenameWouldBreak(t *testing.T) {
	t.Parallel()
	t.Run("a hard-linked file", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		other := filepath.Join(t.TempDir(), "other.json")
		write(t, other, "{}", 0o600)
		require.NoError(t, os.Link(other, filepath.Join(home, codexhooks.FileName)))
		_, err := codexhooks.Install(home)
		require.ErrorIs(t, err, codexhooks.ErrUnwritable)
		assert.Equal(t, "{}", read(t, other))
		_, err = codexhooks.Check(home)
		require.NoError(t, err, "reading a hard-linked file is fine")
	})
	t.Run("a read-only file", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root writes a read-only file")
		}
		home := t.TempDir()
		path := filepath.Join(home, codexhooks.FileName)
		write(t, path, "{}", 0o400)
		_, err := codexhooks.Install(home)
		require.ErrorIs(t, err, codexhooks.ErrUnwritable)
		assert.Equal(t, "{}", read(t, path))
		assertOnly(t, home, codexhooks.FileName)
	})
	t.Run("a read-only file already installed is left alone", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		path := filepath.Join(home, codexhooks.FileName)
		write(t, path, installed, 0o400)
		res, err := codexhooks.Install(home)
		require.NoError(t, err, "nothing to write")
		assert.False(t, res.Changed)
	})
}

// TestInstalledIn: Codex runs every hook only once all three are installed
// and config.toml leaves hooks on; a home that cannot be found, or a hooks
// file that cannot be read, is an error.
func TestInstalledIn(t *testing.T) {
	t.Parallel()
	lookup := func(home string) func(string) (string, bool) {
		return func(k string) (string, bool) { return home, k == "CODEX_HOME" }
	}
	home := t.TempDir()
	installed, err := codexhooks.InstalledIn(lookup(home))
	require.NoError(t, err)
	assert.False(t, installed, "none yet")

	_, err = codexhooks.Install(home)
	require.NoError(t, err)
	installed, err = codexhooks.InstalledIn(lookup(home))
	require.NoError(t, err)
	assert.True(t, installed)

	write(t, filepath.Join(home, "config.toml"), "[features]\nhooks = false\n", 0o600)
	installed, err = codexhooks.InstalledIn(lookup(home))
	require.NoError(t, err)
	assert.False(t, installed, "Codex runs no hook with hooks off")

	_, err = codexhooks.InstalledIn(func(string) (string, bool) { return "", false })
	require.ErrorIs(t, err, codexhooks.ErrNoHome)
	bad := t.TempDir()
	write(t, filepath.Join(bad, codexhooks.FileName), "{not json", 0o600)
	_, err = codexhooks.InstalledIn(lookup(bad))
	require.Error(t, err)
}

// TestHooksOff reports features.hooks = false, under which Codex runs no
// hook at all, ours included.
func TestHooksOff(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		config *string
		off    bool
	}{
		"no config.toml":       {},
		"no features":          {config: new("model = \"o3\"\n")},
		"hooks = true":         {config: new("[features]\nhooks = true\n")},
		"hooks = false":        {config: new("[features]\nhooks = false\n"), off: true},
		"another feature off":  {config: new("[features]\nmemories = false\n")},
		"features not a table": {config: new("features = 1\n")},
		"hooks not a bool":     {config: new("[features]\nhooks = \"no\"\n")},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			if tc.config != nil {
				write(t, filepath.Join(home, "config.toml"), *tc.config, 0o600)
			}
			res, err := codexhooks.Check(home)
			require.NoError(t, err)
			assert.Equal(t, tc.off, res.HooksOff)
			res, err = codexhooks.Install(home)
			require.NoError(t, err, "installing still works: turning hooks on is the user's")
			assert.Equal(t, tc.off, res.HooksOff)
		})
	}
}

func TestInstallFilesystemFailures(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads and writes past file modes")
	}
	t.Run("an unreadable file", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		path := filepath.Join(home, codexhooks.FileName)
		write(t, path, "{}", 0o600)
		require.NoError(t, os.Chmod(path, 0))
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		_, err := codexhooks.Install(home)
		require.ErrorIs(t, err, fs.ErrPermission)
		_, err = codexhooks.Check(home)
		require.ErrorIs(t, err, fs.ErrPermission)
		require.NoError(t, os.Chmod(path, 0o600))
		assert.Equal(t, "{}", read(t, path))
	})
	t.Run("a home it cannot write", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		require.NoError(t, os.Chmod(home, 0o500))
		t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
		_, err := codexhooks.Install(home)
		require.ErrorIs(t, err, fs.ErrPermission)
		require.NoError(t, os.Chmod(home, 0o700))
		assertOnly(t, home)
	})
}

func TestInstallInASymlinkedHome(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	home := filepath.Join(t.TempDir(), ".codex")
	require.NoError(t, os.Symlink(target, home))
	res, err := codexhooks.Install(home)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, codexhooks.FileName), res.Path)
	assert.Equal(t, decode(t, installed), decode(t, read(t, filepath.Join(target, codexhooks.FileName))))
}

// TestInstallAtOnce runs installs side by side: each replaces the whole file
// with a rename, so the file is always whole and ends installed.
func TestInstallAtOnce(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := codexhooks.Install(home)
			errs <- err
		}()
	}
	for range 8 {
		require.NoError(t, <-errs)
	}
	assert.Equal(t, decode(t, installed), decode(t, read(t, filepath.Join(home, codexhooks.FileName))))
	assertOnly(t, home, codexhooks.FileName)
}

// TestInstallReplacesByRename: an install replaces hooks.json by a rename,
// never by writing into it, so a Codex reading it meanwhile reads the old
// file or the new one, never half of either. A reader that opened the old
// file still reads all of it, the path names a new inode, and no temp file
// is left beside it.
func TestInstallReplacesByRename(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, codexhooks.FileName)
	old := `{"hooks":{}}`
	write(t, path, old, 0o600)
	reader, err := os.Open(path) // #nosec G304 -- a temp file the test wrote
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	before := inode(t, path)

	res, err := codexhooks.Install(home)
	require.NoError(t, err)
	require.True(t, res.Changed)
	assert.NotEqual(t, before, inode(t, path), "the file is replaced, not written in place")
	kept, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, old, string(kept), "the old file is untouched under its reader")
	assert.Equal(t, decode(t, installed), decode(t, read(t, path)))
	assertOnly(t, home, codexhooks.FileName)
}

// inode is path's inode number.
func inode(t *testing.T, path string) uint64 {
	t.Helper()
	st, err := os.Stat(path)
	require.NoError(t, err)
	sys, ok := st.Sys().(*syscall.Stat_t)
	require.True(t, ok, "no inode for %s", path)
	return sys.Ino
}

func TestInstallFileModes(t *testing.T) {
	t.Parallel()
	t.Run("a new file is 0600, in a new home of 0700", func(t *testing.T) {
		t.Parallel()
		home := filepath.Join(t.TempDir(), "a", ".codex")
		_, err := codexhooks.Install(home)
		require.NoError(t, err)
		st, err := os.Stat(home)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), st.Mode().Perm())
		st, err = os.Stat(filepath.Join(home, codexhooks.FileName))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
		assertOnly(t, home, codexhooks.FileName)
	})
	t.Run("an existing file keeps its mode", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		path := filepath.Join(home, codexhooks.FileName)
		write(t, path, "{}", 0o600)
		require.NoError(t, os.Chmod(path, 0o640))
		_, err := codexhooks.Install(home)
		require.NoError(t, err)
		st, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o640), st.Mode().Perm())
		assertOnly(t, home, codexhooks.FileName)
	})
}

func TestInstallWritesThroughASymlink(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "codex-hooks.json")
	write(t, target, `{"hooks":{"Stop":[`+userB+`]}}`, 0o600)
	link := filepath.Join(home, codexhooks.FileName)
	require.NoError(t, os.Symlink(target, link))

	res, err := codexhooks.Install(home)
	require.NoError(t, err)
	assert.True(t, res.Changed)
	st, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Equal(t, fs.ModeSymlink, st.Mode().Type(), "the link stays a link")
	assert.Equal(t, decode(t, `{"hooks":{"PostToolUse":[`+ours("PostToolUse", cmd, "")+`],"Stop":[`+userB+`,`+ours("Stop", cmd, "")+`],"PostCompact":[`+ours("PostCompact", cmd, "")+`]}}`), decode(t, read(t, target)))
	assertOnly(t, dotfiles, "codex-hooks.json")

	t.Run("a dangling link is refused", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		link := filepath.Join(home, codexhooks.FileName)
		require.NoError(t, os.Symlink(filepath.Join(home, "gone.json"), link))
		_, err := codexhooks.Install(home)
		require.ErrorIs(t, err, fs.ErrNotExist)
		_, err = os.Stat(filepath.Join(home, "gone.json"))
		require.ErrorIs(t, err, fs.ErrNotExist, "nothing is created behind the link")
		st, err := os.Lstat(link)
		require.NoError(t, err)
		assert.Equal(t, fs.ModeSymlink, st.Mode().Type())
	})
}

func TestInstallLeavesConfigTOMLHooksAlone(t *testing.T) {
	t.Parallel()
	inTOML := `model = "o3"

[[hooks.Stop]]

[[hooks.Stop.hooks]]
type = "command"
command = "herdr-agentisan report codex"
timeout = 10
async = true
`
	tests := map[string]struct {
		config string
		want   error
	}{
		"a report codex hook there": {config: inTOML, want: codexhooks.ErrInConfigTOML},
		"one as an inline table": {
			config: "[hooks]\nPostCompact = [{ hooks = [{ type = \"command\", command = \"/x/herdr-agentisan report codex\" }] }]\n",
			want:   codexhooks.ErrInConfigTOML,
		},
		// Codex runs it on whatever event it sits under, so one there on
		// an event Install never writes would still run twice.
		"one on an event Install never writes": {
			config: "[[hooks.SessionStart]]\n\n[[hooks.SessionStart.hooks]]\ntype = \"command\"\ncommand = \"herdr-agentisan report codex\"\n",
			want:   codexhooks.ErrInConfigTOML,
		},
		"not TOML": {config: "[hooks\n", want: codexhooks.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			config := filepath.Join(home, "config.toml")
			write(t, config, tc.config, 0o600)

			_, err := codexhooks.Install(home)
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), config)
			assert.Equal(t, tc.config, read(t, config), "config.toml is never written")
			assertOnly(t, home, "config.toml")
		})
	}

	t.Run("a config.toml that is not a file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "config.toml"), 0o700))
		fifo := t.TempDir()
		require.NoError(t, syscall.Mkfifo(filepath.Join(fifo, "config.toml"), 0o600))
		for _, home := range []string{dir, fifo} {
			_, err := codexhooks.Install(home)
			require.ErrorIs(t, err, codexhooks.ErrInvalid)
			_, err = codexhooks.Check(home)
			require.ErrorIs(t, err, codexhooks.ErrInvalid)
		}
	})
	t.Run("a config.toml over the cap", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		write(t, filepath.Join(home, "config.toml"), "a = 1\n"+strings.Repeat("\n", codexhooks.MaxFileSize), 0o600)
		_, err := codexhooks.Install(home)
		require.ErrorIs(t, err, codexhooks.ErrInvalid)
	})

	t.Run("other hooks and keys there are fine", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		config := `approvals_reviewer = "auto_review"

[projects."/r"]
trust_level = "trusted"

[[hooks.Stop]]

[[hooks.Stop.hooks]]
type = "command"
command = "notify"

[hooks.state."file:/x:stop:0:0"]
trusted_hash = "sha256:00"
`
		write(t, filepath.Join(home, "config.toml"), config, 0o600)
		before := inode(t, filepath.Join(home, "config.toml"))
		res, err := codexhooks.Install(home)
		require.NoError(t, err)
		assert.True(t, res.Changed)
		assert.Equal(t, config, read(t, filepath.Join(home, "config.toml")))
		assert.Equal(t, before, inode(t, filepath.Join(home, "config.toml")), "not even rewritten with the same bytes")
	})
}

func TestCheck(t *testing.T) {
	t.Parallel()
	stopInTOML := "[[hooks.Stop]]\n\n[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"herdr-agentisan report codex\"\ntimeout = 10\nasync = true\n"
	oldInTOML := "[[hooks.PostToolUse]]\nmatcher = \"*\"\n\n[[hooks.PostToolUse.hooks]]\ntype = \"command\"\ncommand = \"herdr-agentisan report codex\"\ntimeout = 10\n"
	tests := map[string]struct {
		hooks, config *string
		want          map[string]codexhooks.State
		sources       map[string]string // event: file name, "" for none
	}{
		"nothing installed": {
			want:    map[string]codexhooks.State{"PostToolUse": codexhooks.Missing, "Stop": codexhooks.Missing, "PostCompact": codexhooks.Missing},
			sources: map[string]string{"PostToolUse": "", "Stop": "", "PostCompact": ""},
		},
		"installed": {
			hooks:   new(installed),
			want:    map[string]codexhooks.State{"PostToolUse": codexhooks.Present, "Stop": codexhooks.Present, "PostCompact": codexhooks.Present},
			sources: map[string]string{"PostToolUse": codexhooks.FileName, "Stop": codexhooks.FileName, "PostCompact": codexhooks.FileName},
		},
		"present, old and missing": {
			hooks:   new(`{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[{"hooks":[{"type":"command","command":"herdr-agentisan report codex"}]}]}}`),
			want:    map[string]codexhooks.State{"PostToolUse": codexhooks.Present, "Stop": codexhooks.Differs, "PostCompact": codexhooks.Missing},
			sources: map[string]string{"PostToolUse": codexhooks.FileName, "Stop": codexhooks.FileName, "PostCompact": ""},
		},
		"all there, one old": {
			hooks:   new(`{"hooks":{"PostToolUse":[` + ours("PostToolUse", cmd, "") + `],"Stop":[{"hooks":[{"type":"command","command":"herdr-agentisan report codex","timeout":10}]}],"PostCompact":[` + ours("PostCompact", cmd, "") + `]}}`),
			want:    map[string]codexhooks.State{"PostToolUse": codexhooks.Present, "Stop": codexhooks.Differs, "PostCompact": codexhooks.Present},
			sources: map[string]string{"PostToolUse": codexhooks.FileName, "Stop": codexhooks.FileName, "PostCompact": codexhooks.FileName},
		},
		"in config.toml": {
			config:  new(stopInTOML + "\n" + oldInTOML),
			want:    map[string]codexhooks.State{"PostToolUse": codexhooks.Differs, "Stop": codexhooks.Present, "PostCompact": codexhooks.Missing},
			sources: map[string]string{"PostToolUse": "config.toml", "Stop": "config.toml", "PostCompact": ""},
		},
		"hooks.json wins over config.toml": {
			hooks:   new(installed),
			config:  new(oldInTOML),
			want:    map[string]codexhooks.State{"PostToolUse": codexhooks.Present, "Stop": codexhooks.Present, "PostCompact": codexhooks.Present},
			sources: map[string]string{"PostToolUse": codexhooks.FileName, "Stop": codexhooks.FileName, "PostCompact": codexhooks.FileName},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			var files []string
			if tc.hooks != nil {
				write(t, filepath.Join(home, codexhooks.FileName), *tc.hooks, 0o600)
				files = append(files, codexhooks.FileName)
			}
			if tc.config != nil {
				write(t, filepath.Join(home, "config.toml"), *tc.config, 0o600)
				files = append(files, "config.toml")
			}

			res, err := codexhooks.Check(home)
			require.NoError(t, err)
			assert.False(t, res.Changed)
			assert.Equal(t, filepath.Join(home, codexhooks.FileName), res.Path)
			assert.Equal(t, tc.want, states(res))
			assert.Equal(t, []string{"PostToolUse", "Stop", "PostCompact"}, events(res))
			for _, h := range res.Hooks {
				want := ""
				if f := tc.sources[h.Event]; f != "" {
					want = filepath.Join(home, f)
				}
				assert.Equal(t, want, h.Source, h.Event)
			}
			all := true
			for _, s := range tc.want {
				all = all && s == codexhooks.Present
			}
			assert.Equal(t, all, res.Installed())
			assertOnly(t, home, files...)
		})
	}

	t.Run("a missing home is nothing installed, and is not created", func(t *testing.T) {
		t.Parallel()
		home := filepath.Join(t.TempDir(), ".codex")
		res, err := codexhooks.Check(home)
		require.NoError(t, err)
		assert.False(t, res.Installed())
		_, err = os.Stat(home)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})
}

func TestHome(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	write(t, file, "", 0o600)
	tests := map[string]struct {
		env  map[string]string
		want string
		err  bool
	}{
		"CODEX_HOME":                          {env: map[string]string{"CODEX_HOME": dir, "HOME": "/h"}, want: dir},
		"CODEX_HOME cleaned":                  {env: map[string]string{"CODEX_HOME": dir + "/./"}, want: dir},
		"CODEX_HOME empty is unset":           {env: map[string]string{"CODEX_HOME": "", "HOME": "/h"}, want: "/h/.codex"},
		"HOME's .codex, which need not exist": {env: map[string]string{"HOME": "/h"}, want: "/h/.codex"},
		"CODEX_HOME that does not exist":      {env: map[string]string{"CODEX_HOME": filepath.Join(dir, "gone"), "HOME": "/h"}, err: true},
		"CODEX_HOME that is a file":           {env: map[string]string{"CODEX_HOME": file}, err: true},
		"neither":                             {env: map[string]string{}, err: true},
		"HOME relative":                       {env: map[string]string{"HOME": "h"}, err: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := codexhooks.Home(func(k string) (string, bool) {
				v, ok := tc.env[k]
				return v, ok
			})
			if tc.err {
				require.ErrorIs(t, err, codexhooks.ErrNoHome)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestHomeRelativeCodexHomeIsFromTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "ch"), 0o700))
	t.Chdir(dir)
	got, err := codexhooks.Home(func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return "ch", true
		}
		return "", false
	})
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(filepath.Join(dir, "ch"))
	require.NoError(t, err)
	gotReal, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	assert.Equal(t, want, gotReal)
	assert.True(t, filepath.IsAbs(got))
}

func TestErrorsAreDistinct(t *testing.T) {
	t.Parallel()
	all := []error{codexhooks.ErrNoHome, codexhooks.ErrInvalid, codexhooks.ErrAmbiguous, codexhooks.ErrInConfigTOML, codexhooks.ErrUnwritable, codexhooks.ErrNotInstalled}
	for i, a := range all {
		for j, b := range all {
			assert.Equal(t, i == j, errors.Is(a, b), "%v vs %v", a, b)
		}
	}
}
