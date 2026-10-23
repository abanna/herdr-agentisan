package preflight_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/preflight"
)

// slow bounds what a test waits for a run that should end well before it.
const slow = 10 * time.Second

// repo is the fake system's repo, and bin the tdaddy its PATH holds.
const (
	repo = "/work/repo"
	bin  = "/opt/tdaddy/bin/tdaddy"
)

// taskfile has both tdaddy tasks, as herdr-agentisan's Taskfile does.
const taskfile = `version: "3"
tasks:
  build:
    cmds: [go build ./...]
  tdaddy:index:
    cmds: [tdaddy index . --force]
  tdaddy:impact:
    cmds: [tdaddy impact --changed --max-tests 0]
`

// fake is a System over a map of files, with a PATH that holds bin or
// nothing, and a run that answers runErr. The repo is a directory unless
// statErr or files say otherwise. It records what the check asked.
type fake struct {
	files   map[string]string // path -> contents
	dirs    map[string]bool
	statErr map[string]error
	readErr map[string]error
	noBin   bool
	runErr  error

	lookups []string
	reads   []string
	runs    []run
}

type run struct {
	dir, bin string
	args     []string
}

// goRepo is a Go repo that passes: go.mod, a Taskfile.yml with both tasks,
// and a tdaddy that runs.
func goRepo() *fake {
	return &fake{files: map[string]string{
		repo + "/go.mod":       "module example.com/repo\n",
		repo + "/Taskfile.yml": taskfile,
	}}
}

type info struct{ dir bool }

func (i info) Name() string       { return "" }
func (i info) Size() int64        { return 0 }
func (i info) Mode() fs.FileMode  { return map[bool]fs.FileMode{true: fs.ModeDir}[i.dir] }
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.dir }
func (i info) Sys() any           { return nil }

func (f *fake) system() preflight.System {
	return preflight.System{
		LookPath: func(file string) (string, error) {
			f.lookups = append(f.lookups, file)
			if f.noBin {
				return "", errors.New(`exec: "tdaddy": executable file not found in $PATH`)
			}
			return bin, nil
		},
		Stat: func(name string) (fs.FileInfo, error) {
			if err := f.statErr[name]; err != nil {
				return nil, err
			}
			if f.dirs[name] {
				return info{dir: true}, nil
			}
			if _, ok := f.files[name]; ok {
				return info{}, nil
			}
			if name == repo {
				return info{dir: true}, nil
			}
			return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
		},
		ReadFile: func(name string) ([]byte, error) {
			f.reads = append(f.reads, name)
			if err := f.readErr[name]; err != nil {
				return nil, err
			}
			if s, ok := f.files[name]; ok {
				return []byte(s), nil
			}
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		},
		Run: func(ctx context.Context, dir, bin string, args ...string) error {
			f.runs = append(f.runs, run{dir: dir, bin: bin, args: args})
			if err := ctx.Err(); err != nil {
				return err
			}
			return f.runErr
		},
	}
}

// TestTdaddyPassesASetUpGoRepo: tdaddy is found on the PATH and run as
// `tdaddy --version` in the repo, and the Taskfile has both tasks.
func TestTdaddyPassesASetUpGoRepo(t *testing.T) {
	t.Parallel()
	f := goRepo()
	require.NoError(t, preflight.Tdaddy(t.Context(), repo, f.system()))
	assert.Equal(t, []string{"tdaddy"}, f.lookups)
	assert.Equal(t, []run{{dir: repo, bin: bin, args: []string{"--version"}}}, f.runs)
	assert.Equal(t, []string{repo + "/Taskfile.yml"}, f.reads)
}

// TestTdaddySkipsARepoWithoutGoMod: a repo with no go.mod is not a Go repo,
// so nothing else is looked at.
func TestTdaddySkipsARepoWithoutGoMod(t *testing.T) {
	t.Parallel()
	f := &fake{}
	require.NoError(t, preflight.Tdaddy(t.Context(), repo, f.system()))
	assert.Empty(t, f.lookups, "no PATH search")
	assert.Empty(t, f.runs, "no tdaddy run")
	assert.Empty(t, f.reads, "no Taskfile read")
}

// TestTdaddyClasses: every way the check refuses, each with its sentinel and
// a message that says what to fix.
func TestTdaddyClasses(t *testing.T) {
	t.Parallel()
	denied := &fs.PathError{Op: "stat", Path: repo + "/go.mod", Err: fs.ErrPermission}
	tests := map[string]struct {
		repo  string
		setup func(f *fake)
		want  []error
		not   []error
		says  []string
	}{
		"a relative repo": {
			repo: "work/repo", want: []error{preflight.ErrRepo}, says: []string{`"work/repo" is not an absolute path; give the project an absolute repo path`},
		},
		"an empty repo": {
			repo: "", want: []error{preflight.ErrRepo}, says: []string{`"" is not an absolute path`},
		},
		"the repo is gone": {
			setup: func(f *fake) {
				f.statErr = map[string]error{repo: &fs.PathError{Op: "stat", Path: repo, Err: fs.ErrNotExist}}
			},
			want: []error{preflight.ErrRepo, fs.ErrNotExist}, says: []string{"stat " + repo + ": file does not exist; check the project's repo path: it must be a directory the daemon can read"},
		},
		"the repo cannot be checked": {
			setup: func(f *fake) {
				f.statErr = map[string]error{repo: &fs.PathError{Op: "stat", Path: repo, Err: fs.ErrPermission}}
			},
			want: []error{preflight.ErrRepo, fs.ErrPermission}, says: []string{"check the project's repo path: it must be a directory the daemon can read"},
		},
		"the repo is a file": {
			setup: func(f *fake) { f.files[repo] = "" },
			want:  []error{preflight.ErrRepo}, says: []string{repo + " is not a directory; check the project's repo path: it must be a directory the daemon can read"},
		},
		"go.mod cannot be checked": {
			setup: func(f *fake) { f.statErr = map[string]error{repo + "/go.mod": denied} },
			want:  []error{preflight.ErrRepo, fs.ErrPermission}, says: []string{"check the project's repo path: it must be a directory the daemon can read"},
		},
		"go.mod is a directory": {
			setup: func(f *fake) { delete(f.files, repo+"/go.mod"); f.dirs = map[string]bool{repo + "/go.mod": true} },
			want:  []error{preflight.ErrRepo}, says: []string{repo + "/go.mod is a directory, not a module file; remove or rename it"},
		},
		"tdaddy is not on the PATH": {
			setup: func(f *fake) { f.noBin = true },
			want:  []error{preflight.ErrTdaddyMissing}, not: []error{preflight.ErrTdaddyTasks},
			says: []string{"no tdaddy on the daemon's PATH", "executable file not found", "put tdaddy on the PATH herdr starts the daemon with, then restart the daemon"},
		},
		"tdaddy does not run": {
			setup: func(f *fake) { f.runErr = errors.New("exit status 1: mise ERROR No version is set for shim: tdaddy") },
			want:  []error{preflight.ErrTdaddyMissing}, not: []error{preflight.ErrTdaddyTasks},
			says: []string{bin + " --version failed in " + repo, "mise ERROR No version is set for shim: tdaddy", "restart the daemon if the fix changes its PATH"},
		},
		"no Taskfile": {
			setup: func(f *fake) { delete(f.files, repo+"/Taskfile.yml") },
			want:  []error{preflight.ErrTdaddyTasks}, not: []error{preflight.ErrTdaddyMissing},
			says: []string{repo + " has no Taskfile", "add tdaddy:index and tdaddy:impact tasks that run tdaddy"},
		},
		"no tdaddy:index": {
			setup: func(f *fake) {
				f.files[repo+"/Taskfile.yml"] = strings.Replace(taskfile, "tdaddy:index:", "tdaddy:reindex:", 1)
			},
			want: []error{preflight.ErrTdaddyTasks}, says: []string{repo + "/Taskfile.yml has no tdaddy:index task;"},
		},
		"no tdaddy:impact": {
			setup: func(f *fake) {
				f.files[repo+"/Taskfile.yml"] = strings.Replace(taskfile, "tdaddy:impact:", "impact:", 1)
			},
			want: []error{preflight.ErrTdaddyTasks}, says: []string{"has no tdaddy:impact task;"},
		},
		"neither task": {
			setup: func(f *fake) {
				f.files[repo+"/Taskfile.yml"] = "version: \"3\"\ntasks:\n  build:\n    cmds: [go build ./...]\n"
			},
			want: []error{preflight.ErrTdaddyTasks}, says: []string{"has no tdaddy:index or tdaddy:impact task;"},
		},
		"no tasks at all": {
			setup: func(f *fake) { f.files[repo+"/Taskfile.yml"] = "version: \"3\"\n" },
			want:  []error{preflight.ErrTdaddyTasks}, says: []string{"has no tdaddy:index or tdaddy:impact task;"},
		},
		"an empty Taskfile": {
			setup: func(f *fake) { f.files[repo+"/Taskfile.yml"] = "" },
			want:  []error{preflight.ErrTdaddyTasks}, says: []string{"has no tdaddy:index or tdaddy:impact task;"},
		},
		// Only top-level task keys count: a comment, an include named tdaddy,
		// a dependency and a command naming the tasks do not.
		"the tasks named only outside the top-level keys": {
			setup: func(f *fake) {
				f.files[repo+"/Taskfile.yml"] = "version: \"3\"\n# tdaddy:index and tdaddy:impact come from the include\nincludes:\n  tdaddy: ./tdaddy.yml\ntasks:\n  check:\n    deps: [tdaddy:index]\n    cmds: [task tdaddy:impact]\n"
			},
			want: []error{preflight.ErrTdaddyTasks}, says: []string{"has no tdaddy:index or tdaddy:impact task;"},
		},
		"a Taskfile that does not parse": {
			setup: func(f *fake) { f.files[repo+"/Taskfile.yml"] = "tasks: [tdaddy:index, tdaddy:impact]\n" },
			want:  []error{preflight.ErrTdaddyTasks}, says: []string{repo + "/Taskfile.yml does not parse", "fix it so `task --list-all` reads it"},
		},
		"a Taskfile that cannot be read": {
			setup: func(f *fake) {
				f.readErr = map[string]error{repo + "/Taskfile.yml": &fs.PathError{Op: "open", Path: repo + "/Taskfile.yml", Err: fs.ErrPermission}}
			},
			want: []error{preflight.ErrTdaddyTasks, fs.ErrPermission}, says: []string{"make it a regular file the daemon can read"},
		},
		"tdaddy missing and no tasks": {
			setup: func(f *fake) { f.noBin = true; delete(f.files, repo+"/Taskfile.yml") },
			want:  []error{preflight.ErrTdaddyMissing, preflight.ErrTdaddyTasks},
			says:  []string{"no tdaddy on the daemon's PATH", "has no Taskfile"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := goRepo()
			if tc.setup != nil {
				tc.setup(f)
			}
			r := repo
			if tc.setup == nil {
				r = tc.repo
			}
			err := preflight.Tdaddy(t.Context(), r, f.system())
			require.Error(t, err)
			for _, want := range tc.want {
				require.ErrorIs(t, err, want)
			}
			for _, not := range tc.not {
				assert.NotErrorIs(t, err, not)
			}
			for _, s := range tc.says {
				assert.Contains(t, err.Error(), s)
			}
		})
	}
}

// TestTdaddyTaskfileForms: only the tasks' names are checked, so a task in
// Task's one-line form counts, and so does a Taskfile with CRLF line ends
// or a byte-order mark. One that is not UTF-8 does not parse.
func TestTdaddyTaskfileForms(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		taskfile string
		want     error
	}{
		"one-line tasks":    {taskfile: "version: \"3\"\ntasks:\n  tdaddy:index: tdaddy index . --force\n  tdaddy:impact: tdaddy impact --changed --max-tests 0\n"},
		"CRLF line ends":    {taskfile: strings.ReplaceAll(taskfile, "\n", "\r\n")},
		"a byte-order mark": {taskfile: "\ufeff" + taskfile},
		"no final newline":  {taskfile: strings.TrimSuffix(taskfile, "\n")},
		"not UTF-8":         {taskfile: "version: \"3\"\ntasks:\n  tdaddy:index: \xff\xfe\n  tdaddy:impact: x\n", want: preflight.ErrTdaddyTasks},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := goRepo()
			f.files[repo+"/Taskfile.yml"] = tc.taskfile
			err := preflight.Tdaddy(t.Context(), repo, f.system())
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
			assert.Contains(t, err.Error(), "does not parse")
		})
	}
}

// TestTdaddyReadsTheTaskfileTaskWould: each of Task's file names is read,
// the first one there is the Taskfile even when a later one has the tasks,
// and a name Task does not look for is not read.
func TestTdaddyReadsTheTaskfileTaskWould(t *testing.T) {
	t.Parallel()
	names := []string{
		"Taskfile.yml", "taskfile.yml", "Taskfile.yaml", "taskfile.yaml",
		"Taskfile.dist.yml", "taskfile.dist.yml", "Taskfile.dist.yaml", "taskfile.dist.yaml",
	}
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &fake{files: map[string]string{repo + "/go.mod": "module m\n", repo + "/" + name: taskfile}}
			require.NoError(t, preflight.Tdaddy(t.Context(), repo, f.system()))
			if i+1 < len(names) {
				f := &fake{files: map[string]string{
					repo + "/go.mod":        "module m\n",
					repo + "/" + name:       "version: \"3\"\n",
					repo + "/" + names[i+1]: taskfile,
				}}
				err := preflight.Tdaddy(t.Context(), repo, f.system())
				require.ErrorIs(t, err, preflight.ErrTdaddyTasks)
				assert.Contains(t, err.Error(), repo+"/"+name+" has no tdaddy:index or tdaddy:impact task")
			}
		})
	}
	t.Run("a name Task does not look for", func(t *testing.T) {
		t.Parallel()
		f := &fake{files: map[string]string{repo + "/go.mod": "module m\n", repo + "/TASKFILE.yml": taskfile}}
		require.ErrorIs(t, preflight.Tdaddy(t.Context(), repo, f.system()), preflight.ErrTdaddyTasks)
		assert.NotContains(t, f.reads, repo+"/TASKFILE.yml")
	})
}

// TestTdaddyCanceled: a check whose context ends, before the check or while
// tdaddy runs, is that context's error, never a claim that tdaddy is missing.
func TestTdaddyCanceled(t *testing.T) {
	t.Parallel()
	t.Run("before the check", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := preflight.Tdaddy(ctx, repo, goRepo().system())
		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, preflight.ErrTdaddyMissing)
		assert.NotErrorIs(t, err, preflight.ErrTdaddyTasks)
	})
	t.Run("while tdaddy runs", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		sys := goRepo().system()
		sys.Run = func(ctx context.Context, _, _ string, _ ...string) error {
			cancel()
			return ctx.Err()
		}
		err := preflight.Tdaddy(ctx, repo, sys)
		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, preflight.ErrTdaddyMissing)
		assert.NotErrorIs(t, err, preflight.ErrTdaddyTasks)
	})
}

// standIn is the checked-in stand-in tdaddy, a runner whose environment
// holds only what it needs, and the dir it records its run in.
func standIn(t *testing.T, mode string) (string, preflight.Runner, string) {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("testdata", "tdaddy"))
	require.NoError(t, err)
	rec := t.TempDir()
	return bin, preflight.Runner{Env: []string{"FAKE_TDADDY_DIR=" + rec, "FAKE_TDADDY_MODE=" + mode, "PATH=/usr/bin:/bin"}}, rec
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(strings.TrimSpace(path))
	require.NoError(t, err)
	return p
}

// TestRunnerRunsInTheDir: the run happens in the repo, with the args given,
// and exit 0 is no error.
func TestRunnerRunsInTheDir(t *testing.T) {
	t.Parallel()
	bin, r, rec := standIn(t, "ok")
	dir := t.TempDir()
	require.NoError(t, r.Run(t.Context(), dir, bin, "--version"))
	cwd, err := os.ReadFile(filepath.Join(rec, "cwd"))
	require.NoError(t, err)
	assert.Equal(t, realPath(t, dir), realPath(t, string(cwd)))
	args, err := os.ReadFile(filepath.Join(rec, "args"))
	require.NoError(t, err)
	assert.Equal(t, "--version\n", string(args))

	r.Timeout = -time.Second
	require.NoError(t, r.Run(t.Context(), dir, bin, "--version"), "a negative timeout is the default one")
}

// TestRunnerFailures: a run that does not exit 0, or cannot start, is an
// error carrying the first line of what it printed.
func TestRunnerFailures(t *testing.T) {
	t.Parallel()
	notExec := filepath.Join(t.TempDir(), "tdaddy")
	require.NoError(t, os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o600))
	reason := []string{"exit status 1: the real reason"}
	tests := map[string]struct {
		mode string
		env  []string // added to the stand-in's environment
		bin  func(standIn string) string
		want error
		says []string
		not  []string
	}{
		"exits 1": {
			mode: "fail", says: []string{"exit status 1: mise ERROR No version is set for shim: tdaddy"},
			not: []string{"Set a global default"},
		},
		"exits 3, silently": {mode: "silent", says: []string{"exit status 3"}},
		"a long first line": {mode: "long", says: []string{"exit status 1: " + strings.Repeat("0", 200)}, not: []string{strings.Repeat("0", 201)}},
		// 1.4 MB, past any pipe buffer: the output is drained, so the run ends.
		"floods its output":        {mode: "flood", says: []string{"exit status 1: tdaddy"}, not: []string{"tdaddy\ntdaddy"}},
		"bytes that are not UTF-8": {mode: "garbled", says: []string{"exit status 1: bad bytes"}, not: []string{"\ufffd"}},
		"CRLF line ends":           {mode: "crlf", says: []string{"exit status 1: first line"}, not: []string{"\r", "second line"}},
		"blank lines first":        {mode: "blankfirst", says: []string{"exit status 1: the reason after a blank line"}},
		"the reason on stdout":     {mode: "stdout", says: []string{"exit status 1: tdaddy: the reason, on stdout"}},
		// Leading blanks never count toward the 4 KiB kept, however many
		// there are (NERD-5343, Draft 20): 4,096 and up lost the reason.
		"100 blank bytes first":   {mode: "blanks", env: []string{"FAKE_TDADDY_BLANKS=100"}, says: reason},
		"4,000 blank bytes first": {mode: "blanks", env: []string{"FAKE_TDADDY_BLANKS=4000"}, says: reason},
		"4,096 blank bytes first": {mode: "blanks", env: []string{"FAKE_TDADDY_BLANKS=4096"}, says: reason},
		"5,000 blank bytes first": {mode: "blanks", env: []string{"FAKE_TDADDY_BLANKS=5000"}, says: reason},
		// Output that is not blank still counts: a reason past the 4 KiB
		// kept is not seen.
		"the reason past the cap": {mode: "pastcap", says: []string{"exit status 1: " + strings.Repeat("0", 200)}, not: []string{"the real reason"}},
		// x then 150 two-byte é: the cut at 200 bytes falls inside an é.
		"a cut inside a character": {mode: "longutf8", says: []string{"exit status 1: x" + strings.Repeat("é", 99)}, not: []string{strings.Repeat("é", 100)}},
		"no such file": {
			bin: func(string) string { return filepath.Join(t.TempDir(), "tdaddy") }, want: fs.ErrNotExist, says: []string{"start "},
		},
		"not executable": {
			bin: func(string) string { return notExec }, want: fs.ErrPermission, says: []string{"start " + notExec},
		},
		"a relative path": {bin: func(string) string { return "tdaddy" }, says: []string{`no absolute path to run ("tdaddy")`}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bin, r, _ := standIn(t, tc.mode)
			r.Env = append(r.Env, tc.env...)
			if tc.bin != nil {
				bin = tc.bin(bin)
			}
			err := r.Run(t.Context(), t.TempDir(), bin, "--version")
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			for _, s := range tc.says {
				assert.Contains(t, err.Error(), s)
			}
			for _, s := range tc.not {
				assert.NotContains(t, err.Error(), s)
			}
			assert.True(t, utf8.ValidString(err.Error()), "the error is valid UTF-8")
		})
	}
}

// TestRunnerFollowsASymlink: a tdaddy on the PATH that is a symlink, as a
// mise shim is, runs what it points to.
func TestRunnerFollowsASymlink(t *testing.T) {
	t.Parallel()
	bin, r, rec := standIn(t, "ok")
	link := filepath.Join(t.TempDir(), "tdaddy")
	require.NoError(t, os.Symlink(bin, link))
	require.NoError(t, r.Run(t.Context(), t.TempDir(), link, "--version"))
	_, err := os.Stat(filepath.Join(rec, "args"))
	require.NoError(t, err, "the stand-in ran")
}

// recordedChild is the pid the stand-in in rec recorded for its child, read
// once the run has returned, so nothing writes it any more; false when it
// recorded none, as when the run ended it before it started one. The
// stand-in renames its record into place whole (record_child).
func recordedChild(t *testing.T, rec string) (int, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rec, "child"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false
	}
	require.NoError(t, err)
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err, "the stand-in's record holds its child's pid")
	return child, true
}

// childGone waits until the child the stand-in recorded in rec no longer
// exists. The run must have returned, and the stand-in recorded a child.
func childGone(t *testing.T, rec string) {
	t.Helper()
	child, ok := recordedChild(t, rec)
	require.True(t, ok, "the hanging tdaddy recorded its child")
	assert.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
	}, slow, 10*time.Millisecond, "tdaddy's child outlived the run")
}

// killsTheChild runs a hanging stand-in in mode, held back delay seconds
// before it starts its child, through run, which ends the run within limit,
// first 300 ms, until a run ends after the stand-in recorded its child, and
// checks that child is gone. A run that ends the stand-in before it starts
// its child, as a loaded machine can (NERD-5563), shows nothing about what
// the kill reaches, so it is run again with the limit doubled; run checks
// every run's own end.
func killsTheChild(t *testing.T, mode, delay string, run func(r preflight.Runner, bin string, limit time.Duration)) {
	t.Helper()
	limit := 300 * time.Millisecond
	for ; limit < slow; limit *= 2 {
		bin, r, rec := standIn(t, mode)
		r.Env = append(r.Env, "FAKE_TDADDY_DELAY="+delay)
		run(r, bin, limit)
		if _, ok := recordedChild(t, rec); ok {
			childGone(t, rec)
			return
		}
	}
	t.Fatalf("the hanging tdaddy recorded no child before its run ended, within up to %v", limit/2)
}

// TestRunnerTimeoutKillsTheGroup: a tdaddy that hangs is killed at the
// timeout, and so is every process it started, even when they ignore
// SIGTERM. One slow to start its child, past the first timeout, is run again
// until it has one.
func TestRunnerTimeoutKillsTheGroup(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ mode, delay string }{
		"a hang":                         {"hang", "0"},
		"a hang that ignores SIGTERM":    {"stubborn", "0"},
		"a hang slow to start its child": {"hang", "1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			killsTheChild(t, tc.mode, tc.delay, func(r preflight.Runner, bin string, limit time.Duration) {
				r.Timeout = limit
				start := time.Now()
				err := r.Run(t.Context(), t.TempDir(), bin, "--version")
				require.ErrorIs(t, err, preflight.ErrTimeout)
				assert.Contains(t, err.Error(), "no answer within "+limit.String())
				assert.Less(t, time.Since(start), limit+slow)
			})
		})
	}
}

// TestRunnerCancelKillsTheGroup: a run whose context is canceled ends with
// that cancel, and takes everything it started with it.
func TestRunnerCancelKillsTheGroup(t *testing.T) {
	t.Parallel()
	bin, r, rec := standIn(t, "hang")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(rec, "child")); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	err := r.Run(ctx, t.TempDir(), bin, "--version")
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, preflight.ErrTimeout, "a cancel is not a timeout")
	childGone(t, rec)
}

// TestRunnerDrainsItsOutput: a tdaddy that prints far past the cap and then
// exits 0 passes. The output is read to its end and dropped, never left to
// fill the pipe or cut off under the child.
func TestRunnerDrainsItsOutput(t *testing.T) {
	t.Parallel()
	bin, r, _ := standIn(t, "chatty")
	require.NoError(t, r.Run(t.Context(), t.TempDir(), bin, "--version"))
}

// TestRunnerBoundsAnOrphansPipes: a tdaddy that exits 0 but leaves a child
// holding its output does not hold up the run past WaitDelay. That is an
// error, not a pass.
func TestRunnerBoundsAnOrphansPipes(t *testing.T) {
	t.Parallel()
	bin, r, rec := standIn(t, "orphan")
	t.Cleanup(func() {
		if raw, err := os.ReadFile(filepath.Join(rec, "child")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	start := time.Now()
	err := r.Run(t.Context(), t.TempDir(), bin, "--version")
	require.ErrorIs(t, err, exec.ErrWaitDelay)
	assert.Less(t, time.Since(start), slow)
}

// TestRunnerCallersDeadlineIsNotATimeout: when the caller's own deadline
// ends the run before the Runner's timeout, the error is the caller's
// deadline, not ErrTimeout.
func TestRunnerCallersDeadlineIsNotATimeout(t *testing.T) {
	t.Parallel()
	for name, delay := range map[string]string{"a hang": "0", "a hang slow to start its child": "1"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			killsTheChild(t, "hang", delay, func(r preflight.Runner, bin string, limit time.Duration) {
				r.Timeout = 2 * slow
				ctx, cancel := context.WithTimeout(t.Context(), limit)
				defer cancel()
				err := r.Run(ctx, t.TempDir(), bin, "--version")
				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.NotErrorIs(t, err, preflight.ErrTimeout)
			})
		})
	}
}

// TestRunnerInheritsTheEnvironment: with no Env, tdaddy gets the daemon's
// own environment.
func TestRunnerInheritsTheEnvironment(t *testing.T) {
	bin, _, rec := standIn(t, "ok")
	t.Setenv("FAKE_TDADDY_DIR", rec)
	t.Setenv("FAKE_TDADDY_MODE", "fail")
	err := preflight.Runner{}.Run(t.Context(), t.TempDir(), bin, "--version")
	require.Error(t, err, "the inherited mode made it fail")
	assert.Contains(t, err.Error(), "mise ERROR")
}

// TestTdaddyOnARealRepo: the check end to end on a repo on disk, through
// the stand-in tdaddy, found where the injected PATH search says. The repo's
// path is used as it is: with spaces, non-ASCII and shell characters, through
// a symlink, gone, a symlink to nothing, or a file.
func TestTdaddyOnARealRepo(t *testing.T) {
	t.Parallel()
	goRepoAt := func(t *testing.T, dir string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "Taskfile.yml"), []byte(taskfile), 0o600))
	}
	for name, tc := range map[string]struct {
		mode    string
		timeout time.Duration
		repo    func(t *testing.T) string
		want    []error
		says    string
	}{
		"tdaddy runs": {mode: "ok", repo: func(t *testing.T) string { d := t.TempDir(); goRepoAt(t, d); return d }},
		"tdaddy does not run": {
			mode: "fail", repo: func(t *testing.T) string { d := t.TempDir(); goRepoAt(t, d); return d },
			want: []error{preflight.ErrTdaddyMissing}, says: "mise ERROR No version is set for shim: tdaddy",
		},
		"tdaddy hangs": {
			mode: "hang", timeout: 300 * time.Millisecond, repo: func(t *testing.T) string { d := t.TempDir(); goRepoAt(t, d); return d },
			want: []error{preflight.ErrTdaddyMissing, preflight.ErrTimeout}, says: "no answer within 300ms",
		},
		"spaces, non-ASCII and shell characters": {mode: "ok", repo: func(t *testing.T) string {
			d := filepath.Join(t.TempDir(), "my repo é;$(x)|*")
			goRepoAt(t, d)
			return d
		}},
		"through a symlink": {mode: "ok", repo: func(t *testing.T) string {
			d := filepath.Join(t.TempDir(), "real")
			goRepoAt(t, d)
			link := filepath.Join(t.TempDir(), "link")
			require.NoError(t, os.Symlink(d, link))
			return link
		}},
		"gone since the profile resolved": {
			mode: "ok", repo: func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
			want: []error{preflight.ErrRepo, fs.ErrNotExist}, says: "check the project's repo path: it must be a directory the daemon can read",
		},
		"a symlink to nothing": {
			mode: "ok", repo: func(t *testing.T) string {
				link := filepath.Join(t.TempDir(), "link")
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), link))
				return link
			},
			want: []error{preflight.ErrRepo, fs.ErrNotExist}, says: "check the project's repo path: it must be a directory the daemon can read",
		},
		"a file, not a directory": {
			mode: "ok", repo: func(t *testing.T) string {
				f := filepath.Join(t.TempDir(), "repo")
				require.NoError(t, os.WriteFile(f, nil, 0o600))
				return f
			},
			want: []error{preflight.ErrRepo}, says: "is not a directory; check the project's repo path: it must be a directory the daemon can read",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := tc.repo(t)
			bin, r, rec := standIn(t, tc.mode)
			r.Timeout = tc.timeout
			sys := preflight.System{
				LookPath: func(string) (string, error) { return bin, nil },
				Stat:     os.Stat,
				ReadFile: os.ReadFile,
				Run:      r.Run,
			}
			err := preflight.Tdaddy(t.Context(), dir, sys)
			if tc.want == nil {
				require.NoError(t, err)
				if tc.mode == "ok" {
					cwd, err := os.ReadFile(filepath.Join(rec, "cwd"))
					require.NoError(t, err, "tdaddy ran")
					assert.Equal(t, realPath(t, dir), realPath(t, string(cwd)), "in the repo")
				}
				return
			}
			for _, want := range tc.want {
				require.ErrorIs(t, err, want)
			}
			assert.NotErrorIs(t, err, preflight.ErrTdaddyTasks)
			assert.Contains(t, err.Error(), tc.says)
			if errors.Is(err, preflight.ErrRepo) {
				_, err := os.Stat(filepath.Join(rec, "args"))
				assert.ErrorIs(t, err, fs.ErrNotExist, "tdaddy did not run")
			}
		})
	}
}

// TestTdaddyTaskfileThatIsADirectory: a directory at the Taskfile's name
// cannot be read, so the repo's Taskfile is not set up, and the message
// says to make it a file.
func TestTdaddyTaskfileThatIsADirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "Taskfile.yml"), 0o700))
	bin, r, _ := standIn(t, "ok")
	sys := preflight.System{
		LookPath: func(string) (string, error) { return bin, nil },
		Stat:     os.Stat,
		ReadFile: os.ReadFile,
		Run:      r.Run,
	}
	err := preflight.Tdaddy(t.Context(), dir, sys)
	require.ErrorIs(t, err, preflight.ErrTdaddyTasks)
	assert.NotErrorIs(t, err, preflight.ErrTdaddyMissing)
	assert.Contains(t, err.Error(), "is a directory; make it a regular file the daemon can read")
}

// TestHostCheck: the host system is complete, and HostCheck passes a repo
// with no go.mod without searching the PATH or running anything.
func TestHostCheck(t *testing.T) {
	t.Parallel()
	h := preflight.Host()
	assert.NotNil(t, h.LookPath)
	assert.NotNil(t, h.Stat)
	assert.NotNil(t, h.ReadFile)
	assert.NotNil(t, h.Run)
	assert.Equal(t, 10*time.Second, preflight.DefaultTimeout, "A34's timeout")
	var check preflight.Check = preflight.HostCheck
	require.NoError(t, check(t.Context(), t.TempDir()))
}

// TestHostCheckInAGoRepo: HostCheck, through Host(), in a Go repo on disk.
// PATH is a temp dir holding the stand-in as tdaddy, so the host's own PATH
// search finds it and nothing searches the real PATH or runs the real
// tdaddy. Not parallel: it sets PATH.
func TestHostCheckInAGoRepo(t *testing.T) {
	standIn, err := filepath.Abs(filepath.Join("testdata", "tdaddy"))
	require.NoError(t, err)
	pathDir := t.TempDir()
	require.NoError(t, os.Symlink(standIn, filepath.Join(pathDir, "tdaddy")))
	rec := t.TempDir()
	t.Setenv("PATH", pathDir)
	t.Setenv("FAKE_TDADDY_DIR", rec)
	repoAt := func(t *testing.T, tf string) string {
		t.Helper()
		d := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(d, "go.mod"), []byte("module m\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(d, "Taskfile.yml"), []byte(tf), 0o600))
		return d
	}
	t.Run("set up", func(t *testing.T) {
		t.Setenv("FAKE_TDADDY_MODE", "ok")
		d := repoAt(t, taskfile)
		require.NoError(t, preflight.HostCheck(t.Context(), d))
		cwd, err := os.ReadFile(filepath.Join(rec, "cwd"))
		require.NoError(t, err, "tdaddy ran")
		assert.Equal(t, realPath(t, d), realPath(t, string(cwd)), "in the repo")
		args, err := os.ReadFile(filepath.Join(rec, "args"))
		require.NoError(t, err)
		assert.Equal(t, "--version\n", string(args))
	})
	t.Run("tdaddy does not run", func(t *testing.T) {
		t.Setenv("FAKE_TDADDY_MODE", "fail")
		err := preflight.HostCheck(t.Context(), repoAt(t, taskfile))
		require.ErrorIs(t, err, preflight.ErrTdaddyMissing)
		assert.NotErrorIs(t, err, preflight.ErrTdaddyTasks)
		assert.Contains(t, err.Error(), filepath.Join(pathDir, "tdaddy")+" --version failed")
		assert.Contains(t, err.Error(), "mise ERROR No version is set for shim: tdaddy")
	})
	t.Run("no tasks", func(t *testing.T) {
		t.Setenv("FAKE_TDADDY_MODE", "ok")
		err := preflight.HostCheck(t.Context(), repoAt(t, "version: \"3\"\n"))
		require.ErrorIs(t, err, preflight.ErrTdaddyTasks)
		assert.NotErrorIs(t, err, preflight.ErrTdaddyMissing)
	})
	t.Run("not on the PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		err := preflight.HostCheck(t.Context(), repoAt(t, taskfile))
		require.ErrorIs(t, err, preflight.ErrTdaddyMissing)
		assert.Contains(t, err.Error(), "no tdaddy on the daemon's PATH")
	})
	// Host's own os.Stat follows links: a repo reached through one is still
	// a Go repo, and a go.mod that is a link to nothing counts as none (A34).
	t.Run("a repo reached through a symlink", func(t *testing.T) {
		t.Setenv("FAKE_TDADDY_MODE", "ok")
		own := t.TempDir()
		t.Setenv("FAKE_TDADDY_DIR", own)
		link := filepath.Join(t.TempDir(), "link")
		require.NoError(t, os.Symlink(repoAt(t, taskfile), link))
		require.NoError(t, preflight.HostCheck(t.Context(), link))
		_, err := os.Stat(filepath.Join(own, "args"))
		require.NoError(t, err, "tdaddy ran")
	})
	t.Run("a go.mod that is a symlink to nothing", func(t *testing.T) {
		t.Setenv("FAKE_TDADDY_MODE", "ok")
		own := t.TempDir()
		t.Setenv("FAKE_TDADDY_DIR", own)
		d := t.TempDir()
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(d, "go.mod")))
		require.NoError(t, preflight.HostCheck(t.Context(), d))
		_, err := os.Stat(filepath.Join(own, "args"))
		require.ErrorIs(t, err, fs.ErrNotExist, "tdaddy did not run")
	})
}
