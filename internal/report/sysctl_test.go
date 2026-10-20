package report_test

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/report"
)

// procargs2 builds what darwin's kern.procargs2 sysctl returns: argc as a
// native (little-endian) int32, the exec path and its NUL, pad more NULs for
// alignment, then argv and env, each string followed by a NUL.
func procargs2(argc int32, execPath string, pad int, argv, env []string) []byte {
	raw := binary.LittleEndian.AppendUint32(nil, uint32(argc))
	raw = append(raw, execPath...)
	raw = append(raw, make([]byte, 1+pad)...)
	for _, s := range append(append([]string{}, argv...), env...) {
		raw = append(append(raw, s...), 0)
	}
	return raw
}

// once is a kern.procargs2 reader that returns raw on every read.
func once(raw []byte) func(int) ([]byte, error) {
	return func(int) ([]byte, error) { return raw, nil }
}

// TestProcArgs2CmdlineClasses walks what kern.procargs2 can hold. The exec
// path is followed by its NUL and up to 7 NULs of alignment, which the
// kernel itself skips, so 8 or more after the terminator can only hide an
// empty argument: that is refused rather than misread. A target whose
// environment the kernel withholds (CS_RESTRICT) ends right after argv.
func TestProcArgs2CmdlineClasses(t *testing.T) {
	t.Parallel()
	env := []string{"HOME=/Users/u", "TERM=xterm"}
	codex := []string{"/opt/homebrew/bin/codex", "--no-daemon"}
	tests := map[string]struct {
		raw  []byte
		want []string
	}{
		"the pane's codex":                     {raw: procargs2(2, "/opt/homebrew/bin/codex", 0, codex, env), want: codex},
		"seven bytes of alignment":             {raw: procargs2(2, "/opt/homebrew/bin/codex", 7, codex, env), want: codex},
		"no environment (a restricted target)": {raw: procargs2(2, "/opt/homebrew/bin/codex", 3, codex, nil), want: codex},
		"no arguments":                         {raw: procargs2(0, "/usr/libexec/x", 2, nil, env), want: []string{}},
		"an empty argument after the first":    {raw: procargs2(4, "/bin/sh", 1, []string{"sh", "-c", "", "echo"}, env), want: []string{"sh", "-c", "", "echo"}},
		"argument bytes that are not UTF-8":    {raw: procargs2(2, "/bin/x", 0, []string{"\xff\xfe", "app-server"}, nil), want: []string{"\xff\xfe", "app-server"}},
		// argv[0] is the name the Linux path compares, too, and unlike the
		// kinfo p_comm it is never cut at MAXCOMLEN (16 bytes).
		"an argv[0] past 16 bytes": {
			raw:  procargs2(1, "/Users/u/.local/share/mise/installs/node/22/bin/node", 4, []string{"/Users/u/.local/share/mise/installs/node/22/bin/codex"}, env),
			want: []string{"/Users/u/.local/share/mise/installs/node/22/bin/codex"},
		},
		// The kernel's exec path and argv[0] can differ; argv[0] is the one
		// /proc/<pid>/cmdline gives on Linux.
		"an exec path that is not argv[0]": {raw: procargs2(1, "/opt/homebrew/bin/codex", 0, []string{"codex-code-mode-host"}, env), want: []string{"codex-code-mode-host"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.ProcArgs2Cmdline(once(tc.raw))(900)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestProcArgs2CmdlineRefusesWhatItCannotParse: every byte layout the kernel
// never writes, or one that leaves an argument ambiguous, is ErrProcCmdline,
// so a host it cannot read is unverified rather than misnamed.
func TestProcArgs2CmdlineRefusesWhatItCannotParse(t *testing.T) {
	t.Parallel()
	codex := []string{"codex", "--no-daemon"}
	unterminated := procargs2(2, "/opt/homebrew/bin/codex", 0, codex, nil)
	tests := map[string][]byte{
		"nothing":                          nil,
		"shorter than argc":                {2, 0},
		"argc alone":                       binary.LittleEndian.AppendUint32(nil, 0),
		"an exec path with no terminator":  append(binary.LittleEndian.AppendUint32(nil, 1), "/bin/codex"...),
		"an empty exec path":               procargs2(1, "", 0, []string{"codex"}, nil),
		"eight bytes after the terminator": procargs2(2, "/opt/homebrew/bin/codex", 8, codex, nil),
		"a negative argc":                  procargs2(-1, "/opt/homebrew/bin/codex", 0, codex, nil),
		"argc past the bytes left":         procargs2(math.MaxInt32, "/opt/homebrew/bin/codex", 0, codex, nil),
		"fewer arguments than argc":        procargs2(3, "/opt/homebrew/bin/codex", 0, codex, nil),
		"the last argument unterminated":   unterminated[:len(unterminated)-1],
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := report.ProcArgs2Cmdline(once(raw))(900)
			require.ErrorIs(t, err, report.ErrProcCmdline)
		})
	}
}

// TestProcArgs2CmdlineReadsTwice: the sysctl sizes its answer in one call
// and fills it in another, so an exec between them returns a slice of the
// new arguments that does not start at argc. The arguments count only when
// two reads agree byte for byte.
func TestProcArgs2CmdlineReadsTwice(t *testing.T) {
	t.Parallel()
	codex := procargs2(2, "/opt/homebrew/bin/codex", 0, []string{"codex", "--no-daemon"}, nil)
	other := procargs2(2, "/opt/homebrew/bin/codey", 0, []string{"codey", "--no-daemon"}, nil)
	require.Len(t, other, len(codex))
	gone := errors.New("no such process")

	tests := map[string]struct {
		reads [][]byte
		errs  []error
	}{
		"the arguments changed between reads": {reads: [][]byte{codex, other}},
		"the second read fails":               {reads: [][]byte{codex, nil}, errs: []error{nil, gone}},
		"the first read fails":                {reads: [][]byte{nil, codex}, errs: []error{gone, nil}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			n := 0
			read := func(int) ([]byte, error) {
				mu.Lock()
				defer mu.Unlock()
				i := min(n, len(tc.reads)-1)
				n++
				if i < len(tc.errs) && tc.errs[i] != nil {
					return nil, tc.errs[i]
				}
				return tc.reads[i], nil
			}
			_, err := report.ProcArgs2Cmdline(read)(900)
			require.ErrorIs(t, err, report.ErrProcCmdline)
		})
	}

	t.Run("two reads that agree", func(t *testing.T) {
		t.Parallel()
		n := 0
		got, err := report.ProcArgs2Cmdline(func(int) ([]byte, error) { n++; return codex, nil })(900)
		require.NoError(t, err)
		assert.Equal(t, []string{"codex", "--no-daemon"}, got)
		assert.Equal(t, 2, n, "read exactly twice")
	})
}

// TestKinfoStatClasses: a kinfo_proc gives the parent and the start time,
// the wall-clock microsecond the process forked, kept across exec. A record
// for another pid, a negative parent or an impossible time is ErrProcStat.
func TestKinfoStatClasses(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		kinfo report.Kinfo
		want  report.Stat
		err   bool
	}{
		"a process":                   {kinfo: report.Kinfo{PID: 900, PPID: 600, StartSec: 1_760_900_000, StartUsec: 123_456}, want: report.Stat{PPID: 600, Start: 1_760_900_000_123_456}},
		"the same second, a later µs": {kinfo: report.Kinfo{PID: 900, PPID: 600, StartSec: 1_760_900_000, StartUsec: 123_457}, want: report.Stat{PPID: 600, Start: 1_760_900_000_123_457}},
		"launchd's child":             {kinfo: report.Kinfo{PID: 900, PPID: 1, StartSec: 1, StartUsec: 0}, want: report.Stat{PPID: 1, Start: 1_000_000}},
		"launchd itself":              {kinfo: report.Kinfo{PID: 900, PPID: 0, StartSec: 0, StartUsec: 999_999}, want: report.Stat{PPID: 0, Start: 999_999}},
		"another pid's record":        {kinfo: report.Kinfo{PID: 901, PPID: 600, StartSec: 1, StartUsec: 0}, err: true},
		"a negative parent":           {kinfo: report.Kinfo{PID: 900, PPID: -1, StartSec: 1, StartUsec: 0}, err: true},
		"a negative second":           {kinfo: report.Kinfo{PID: 900, PPID: 600, StartSec: -1, StartUsec: 0}, err: true},
		"a negative microsecond":      {kinfo: report.Kinfo{PID: 900, PPID: 600, StartSec: 1, StartUsec: -1}, err: true},
		"a whole second of µs":        {kinfo: report.Kinfo{PID: 900, PPID: 600, StartSec: 1, StartUsec: 1_000_000}, err: true},
		"a second that overflows":     {kinfo: report.Kinfo{PID: 900, PPID: 600, StartSec: math.MaxInt64, StartUsec: 0}, err: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.KinfoStat(func(int) (report.Kinfo, error) { return tc.kinfo, nil })(900)
			if tc.err {
				require.ErrorIs(t, err, report.ErrProcStat)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("an unreadable process", func(t *testing.T) {
		t.Parallel()
		_, err := report.KinfoStat(func(int) (report.Kinfo, error) { return report.Kinfo{}, syscall.ESRCH })(900)
		require.ErrorIs(t, err, report.ErrProcStat)
		require.ErrorIs(t, err, syscall.ESRCH)
	})
}

// darwinPane is a macOS pane running Codex embedded (--no-daemon), as the
// darwin readers see it: report (5000) -> sh -lc (4998) -> codex (900) ->
// the pane's zsh (600) -> the herdr server (100), which launchd adopted.
type darwinPane struct {
	kinfo map[int]report.Kinfo
	args  map[int][]byte
}

func newDarwinPane() darwinPane {
	env := []string{"HOME=/Users/u"}
	return darwinPane{
		kinfo: map[int]report.Kinfo{
			5000: {PID: 5000, PPID: 4998, StartSec: 900}, 4998: {PID: 4998, PPID: 900, StartSec: 800},
			900: {PID: 900, PPID: 600, StartSec: 500}, 600: {PID: 600, PPID: 100, StartSec: 400},
			100: {PID: 100, PPID: 1, StartSec: 100},
		},
		args: map[int][]byte{
			5000: procargs2(3, "/opt/homebrew/bin/herdr-agentisan", 0, []string{"herdr-agentisan", "report", "codex"}, env),
			4998: procargs2(3, "/bin/sh", 1, []string{"/bin/sh", "-lc", "herdr-agentisan report codex"}, env),
			900:  procargs2(2, "/opt/homebrew/bin/codex", 0, []string{"codex", "--no-daemon"}, env),
			600:  procargs2(1, "/bin/zsh", 0, []string{"-zsh"}, env),
			100:  procargs2(2, "/opt/homebrew/bin/herdr", 1, []string{"herdr", "server"}, env),
		},
	}
}

// lineage reads the pane's lineage from the report through the darwin
// readers. A pid with no procargs2 entry reads as another user's process,
// which the kernel refuses with EINVAL.
func (d darwinPane) lineage() *report.Lineage {
	kinfo := func(pid int) (report.Kinfo, error) {
		k, ok := d.kinfo[pid]
		if !ok {
			return report.Kinfo{}, syscall.ESRCH
		}
		return k, nil
	}
	args := func(pid int) ([]byte, error) {
		raw, ok := d.args[pid]
		if !ok {
			return nil, syscall.EINVAL
		}
		return raw, nil
	}
	return report.ReadLineage(5000, report.KinfoStat(kinfo)).WithCmdline(report.ProcArgs2Cmdline(args))
}

// TestCheckCodexHostThroughTheDarwinReaders: on macOS the host check gives
// the answers it gives on Linux, because it compares the same name, argv[0].
// The exec path, and the kinfo p_comm cut at 16 bytes, would not: a Codex
// helper the codex binary runs under another name would count as Codex, and
// a codex reached by a long path would not.
func TestCheckCodexHostThroughTheDarwinReaders(t *testing.T) {
	t.Parallel()
	env := []string{"HOME=/Users/u"}
	tests := map[string]struct {
		args  map[int][]byte
		want  error
		cause error
	}{
		"the pane's own codex": {},
		"codex by a path past 16 bytes": {args: map[int][]byte{
			900: procargs2(2, "/Users/u/.local/share/mise/installs/node/22/bin/codex", 0, []string{"/Users/u/.local/share/mise/installs/node/22/bin/codex", "--no-daemon"}, env),
		}},
		"codex under the node launcher": {args: map[int][]byte{
			900: procargs2(3, "/opt/homebrew/bin/node", 2, []string{"node", "/opt/homebrew/lib/node_modules/@openai/codex/bin/codex.js", "--no-daemon"}, env),
		}},
		"a helper the codex binary runs": {args: map[int][]byte{
			900: procargs2(1, "/opt/homebrew/bin/codex", 0, []string{"codex-code-mode-host"}, env),
		}, want: report.ErrNoCodexHost},
		"a helper whose name starts with codex": {args: map[int][]byte{
			900: procargs2(2, "/opt/homebrew/bin/codex-linux-sandbox", 0, []string{"/opt/homebrew/bin/codex-linux-sandbox", "--"}, env),
		}, want: report.ErrNoCodexHost},
		"the shared app-server": {args: map[int][]byte{
			4998: procargs2(3, "/opt/homebrew/bin/codex", 0, []string{"codex", "app-server", "--listen"}, env),
		}, want: report.ErrCodexDaemon},
		"an ancestor of another user": {args: map[int][]byte{100: nil}, want: report.ErrHostUnverified, cause: syscall.EINVAL},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pane := newDarwinPane()
			for pid, raw := range tc.args {
				if raw == nil {
					delete(pane.args, pid)
					continue
				}
				pane.args[pid] = raw
			}
			l := pane.lineage()
			require.Equal(t, []int{5000, 4998, 900, 600, 100}, l.PIDs())

			err := l.CheckCodexHost()
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
			if tc.cause != nil {
				require.ErrorIs(t, err, tc.cause)
			}
		})
	}
}
