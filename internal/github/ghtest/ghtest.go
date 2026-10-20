// Package ghtest is a stand-in for the gh CLI in tests: a shell script, kept
// beside this file, that records every call and answers as the test says.
// Tests run it in place of gh, with an environment they build, so no test
// finds the real gh, hands it a token or reaches GitHub.
//
// The script is checked in rather than written per test: running a file
// that was just written races a concurrent fork holding it open (ETXTBSY).
package ghtest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The modes the stand-in answers in. The zero mode answers `pr` with the PR
// list and `api` with the issue count.
const (
	// ModeFail writes the stderr Fail set and exits 1.
	ModeFail = "fail"
	// ModeHang starts a child, records its pid and waits on it forever.
	ModeHang = "hang"
	// ModeWarn writes a warning to stderr, then answers as the zero mode.
	ModeWarn = "warn"
	// ModeFlood prints without end.
	ModeFlood = "flood"
	// ModeKill dies of SIGKILL.
	ModeKill = "kill"
)

// Fake is one stand-in gh: the script, and the directory it records in and
// answers from.
type Fake struct {
	// Bin is the script's absolute path, gh's stand-in.
	Bin string
	// Dir is where it records each call and reads its answers.
	Dir string
}

// New returns a stand-in that answers no open PRs and no open issues.
func New(t testing.TB) *Fake {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("ghtest: cannot find the stand-in script")
	}
	f := &Fake{Bin: filepath.Join(filepath.Dir(file), "gh"), Dir: t.TempDir()}
	f.Answer(t, "[]\n", `{"data":{"repository":{"issues":{"totalCount":0}}}}`+"\n")
	return f
}

// Env is the whole environment a test runs the stand-in with: a PATH for
// the tools it uses and where it records. Nothing of the test's own
// environment, a GH_TOKEN above all, reaches it.
func (f *Fake) Env(extra ...string) []string {
	return append([]string{"PATH=/usr/bin:/bin", "FAKE_GH_DIR=" + f.Dir}, extra...)
}

// Answer sets what `gh pr list` and `gh api graphql` print from now on.
func (f *Fake) Answer(t testing.TB, prs, issues string) {
	t.Helper()
	f.write(t, "prs", prs)
	f.write(t, "issues", issues)
}

// Mode sets how the stand-in answers from now on; "" answers normally.
func (f *Fake) Mode(t testing.TB, mode string) {
	t.Helper()
	f.write(t, "mode", mode)
}

// Fail makes every call from now on write stderr and exit 1.
func (f *Fake) Fail(t testing.TB, stderr string) {
	t.Helper()
	f.write(t, "stderr", stderr)
	f.Mode(t, ModeFail)
}

// Calls are the verbs of every call so far, in order: "pr" or "api".
func (f *Fake) Calls(t testing.TB) []string {
	t.Helper()
	return f.lines(t, "calls")
}

// Args are the arguments of the last call with verb, the verb first.
func (f *Fake) Args(t testing.TB, verb string) []string {
	t.Helper()
	return f.lines(t, "args."+verb)
}

// Seen is the environment of the last call, as the variables the stand-in
// names: GH_PROMPT_DISABLED, NO_COLOR, GH_NO_UPDATE_NOTIFIER and PROBE, and
// TTY, the last of its stdin, stdout and stderr that was a terminal, or
// "none".
func (f *Fake) Seen(t testing.TB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range f.lines(t, "env") {
		k, v, _ := strings.Cut(l, "=")
		out[k] = v
	}
	return out
}

// Child is the pid file a hanging call writes its child's pid to.
func (f *Fake) Child() string {
	return filepath.Join(f.Dir, "child")
}

// write replaces name atomically, so a call running meanwhile reads the old
// answer or the new one, never half of one.
func (f *Fake) write(t testing.TB, name, body string) {
	t.Helper()
	tmp := filepath.Join(f.Dir, "."+name)
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(f.Dir, name)); err != nil {
		t.Fatal(err)
	}
}

// lines reads name's lines; a file not written yet has none.
func (f *Fake) lines(t testing.TB, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.Dir, name)) // #nosec G304 -- name is one of this package's own record files, in the test's temp dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}
