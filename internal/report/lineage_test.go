package report_test

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/report"
)

// procTable is a fake process table: pid -> parent and start time. A pid with
// no entry cannot be read, as when the process has exited. It is safe to
// change while a report reads it, so a test can make a process exit or its
// pid be reused part-way through.
type procTable struct {
	mu    sync.Mutex
	procs map[int]report.Stat
}

func newProcTable(procs map[int]report.Stat) *procTable {
	return &procTable{procs: procs}
}

func (p *procTable) stat(pid int) (report.Stat, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.procs[pid]
	if !ok {
		return report.Stat{}, errors.New("no such process")
	}
	return s, nil
}

func (p *procTable) set(pid int, s report.Stat) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.procs[pid] = s
}

func (p *procTable) exit(pid int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.procs, pid)
}

// pids lists a lineage's pids, nearest first.
func pids(l *report.Lineage) []int {
	out := []int{}
	for _, p := range l.Procs {
		out = append(out, p.PID)
	}
	return out
}

// TestReadLineageClasses walks the shapes a process table can take. A lineage
// never fails: whatever cannot be read shortens it, and a shorter lineage
// matches fewer panes, so it can only send a report nowhere, never elsewhere.
func TestReadLineageClasses(t *testing.T) {
	t.Parallel()

	long := map[int]report.Stat{}
	for pid := 1000; pid < 1000+2*report.MaxLineage; pid++ {
		long[pid] = report.Stat{PPID: pid + 1, Start: 1}
	}

	tests := map[string]struct {
		pid   int
		table map[int]report.Stat
		want  []int
	}{
		// report -> statusline.sh -> sh -c -> claude -> pane shell -> herdr -> init
		"statusline chain up to init": {
			pid: 5000,
			table: map[int]report.Stat{
				5000: {PPID: 4999, Start: 90}, 4999: {PPID: 4998, Start: 80}, 4998: {PPID: 900, Start: 80},
				900: {PPID: 600, Start: 50}, 600: {PPID: 100, Start: 40}, 100: {PPID: 1, Start: 10},
			},
			want: []int{5000, 4999, 4998, 900, 600, 100},
		},
		"re-parented to init once the statusline exited": {
			pid: 5000, table: map[int]report.Stat{5000: {PPID: 1, Start: 90}}, want: []int{5000},
		},
		"an ancestor that cannot be read ends the chain": {
			pid: 5000, table: map[int]report.Stat{5000: {PPID: 4999, Start: 90}, 4999: {PPID: 4998, Start: 80}}, want: []int{5000, 4999},
		},
		"the process itself cannot be read": {pid: 5000, table: map[int]report.Stat{}, want: []int{}},
		// A parent that started after its child is a different process that
		// took the pid of the real one after it exited.
		"a parent that started after its child is a reused pid": {
			pid: 5000, table: map[int]report.Stat{5000: {PPID: 900, Start: 90}, 900: {PPID: 600, Start: 95}, 600: {PPID: 1, Start: 40}},
			want: []int{5000},
		},
		"a parent that started in the same tick is kept": {
			pid: 5000, table: map[int]report.Stat{5000: {PPID: 900, Start: 90}, 900: {PPID: 1, Start: 90}}, want: []int{5000, 900},
		},
		"parent 0 (a kernel thread's) stops": {pid: 5000, table: map[int]report.Stat{5000: {PPID: 0, Start: 9}}, want: []int{5000}},
		"a negative parent stops":            {pid: 5000, table: map[int]report.Stat{5000: {PPID: -3, Start: 9}}, want: []int{5000}},
		"a process that is its own parent":   {pid: 5000, table: map[int]report.Stat{5000: {PPID: 5000, Start: 9}}, want: []int{5000}},
		"a cycle stops at the first repeat": {
			pid:   5000,
			table: map[int]report.Stat{5000: {PPID: 4999, Start: 9}, 4999: {PPID: 4998, Start: 9}, 4998: {PPID: 5000, Start: 9}},
			want:  []int{5000, 4999, 4998},
		},
		"init itself has no lineage":    {pid: 1, table: map[int]report.Stat{1: {PPID: 0}}, want: []int{}},
		"pid 0 has no lineage":          {pid: 0, table: map[int]report.Stat{}, want: []int{}},
		"a negative pid has no lineage": {pid: -1, table: map[int]report.Stat{}, want: []int{}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := report.ReadLineage(tc.pid, newProcTable(tc.table).stat)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, pids(got))
			assert.Equal(t, tc.want, got.PIDs())
		})
	}

	t.Run("a chain deeper than MaxLineage is cut there", func(t *testing.T) {
		t.Parallel()
		got := pids(report.ReadLineage(1000, newProcTable(long).stat))
		require.Len(t, got, report.MaxLineage)
		assert.Equal(t, 1000, got[0])
		assert.Equal(t, 1000+report.MaxLineage-1, got[report.MaxLineage-1])
	})

	t.Run("each process keeps the start time it was read with", func(t *testing.T) {
		t.Parallel()
		got := report.ReadLineage(5000, newProcTable(map[int]report.Stat{5000: {PPID: 900, Start: 90}, 900: {PPID: 1, Start: 50}}).stat)
		assert.Equal(t, []report.Proc{{PID: 5000, Start: 90}, {PID: 900, Start: 50}}, got.Procs)
	})
}

// TestReadLineageAnchors: an anchor is a process the reporting one descends
// from by construction, named by its environment (CLAUDE_PID). Claude runs the
// statusline under setsid and the statusline backgrounds the report, so a
// statusline that exits first leaves the report re-parented to init with a
// lineage of itself alone; the anchor still reaches the pane. It is admitted
// only when it is a readable process other than init that started no later
// than the report: a pid reissued after the anchor exited starts later.
func TestReadLineageAnchors(t *testing.T) {
	t.Parallel()

	reparented := func() map[int]report.Stat {
		return map[int]report.Stat{
			5000: {PPID: 1, Start: 900},   // the report, adopted by init
			900:  {PPID: 600, Start: 500}, // claude
			600:  {PPID: 100, Start: 400}, // the pane shell
			2:    {PPID: 0, Start: 0},
		}
	}

	tests := map[string]struct {
		table   map[int]report.Stat
		anchors []int
		want    []report.Proc
	}{
		"a re-parented report reaches claude through the anchor": {
			table: reparented(), anchors: []int{900},
			want: []report.Proc{{PID: 5000, Start: 900}, {PID: 900, Start: 500}},
		},
		"an anchor that started in the same tick is admitted": {
			table: map[int]report.Stat{5000: {PPID: 1, Start: 900}, 900: {PPID: 1, Start: 900}}, anchors: []int{900},
			want: []report.Proc{{PID: 5000, Start: 900}, {PID: 900, Start: 900}},
		},
		"an anchor that started after the report is a reused pid": {
			table: map[int]report.Stat{5000: {PPID: 1, Start: 900}, 900: {PPID: 1, Start: 901}}, anchors: []int{900},
			want: []report.Proc{{PID: 5000, Start: 900}},
		},
		"an anchor already in the chain is not repeated": {
			table: map[int]report.Stat{5000: {PPID: 900, Start: 900}, 900: {PPID: 1, Start: 500}}, anchors: []int{900},
			want: []report.Proc{{PID: 5000, Start: 900}, {PID: 900, Start: 500}},
		},
		"an anchor that cannot be read is ignored": {
			table: reparented(), anchors: []int{4321},
			want: []report.Proc{{PID: 5000, Start: 900}},
		},
		"init is never an anchor":  {table: reparented(), anchors: []int{1}, want: []report.Proc{{PID: 5000, Start: 900}}},
		"pid 0 is never an anchor": {table: reparented(), anchors: []int{0}, want: []report.Proc{{PID: 5000, Start: 900}}},
		"a negative anchor is ignored": {
			table: reparented(), anchors: []int{-900}, want: []report.Proc{{PID: 5000, Start: 900}},
		},
		"the report itself is not repeated as an anchor": {
			table: reparented(), anchors: []int{5000}, want: []report.Proc{{PID: 5000, Start: 900}},
		},
		"with the report unreadable there is nothing to anchor": {
			table: map[int]report.Stat{900: {PPID: 600, Start: 500}}, anchors: []int{900},
			want: []report.Proc{},
		},
		"several anchors are each admitted once": {
			table: reparented(), anchors: []int{900, 600, 900},
			want: []report.Proc{{PID: 5000, Start: 900}, {PID: 900, Start: 500}, {PID: 600, Start: 400}},
		},
		"no anchors": {table: reparented(), want: []report.Proc{{PID: 5000, Start: 900}}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := report.ReadLineage(5000, newProcTable(tc.table).stat, tc.anchors...)
			assert.Equal(t, tc.want, got.Procs)
		})
	}
}

// TestAnchorsFromTheEnvironment walks what CLAUDE_PID can hold. Claude Code
// sets it to its own pid in every child's environment (it overwrites an
// inherited one); anything that is not a decimal pid names no anchor.
func TestAnchorsFromTheEnvironment(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		env  map[string]string
		want []int
	}{
		"set by claude":           {env: map[string]string{"CLAUDE_PID": "894377"}, want: []int{894377}},
		"unset":                   {env: map[string]string{}, want: nil},
		"empty":                   {env: map[string]string{"CLAUDE_PID": ""}, want: nil},
		"not a number":            {env: map[string]string{"CLAUDE_PID": "claude"}, want: nil},
		"surrounding whitespace":  {env: map[string]string{"CLAUDE_PID": " 894377\n"}, want: nil},
		"a sign":                  {env: map[string]string{"CLAUDE_PID": "+894377"}, want: nil},
		"negative":                {env: map[string]string{"CLAUDE_PID": "-5"}, want: nil},
		"overflows an int":        {env: map[string]string{"CLAUDE_PID": "99999999999999999999"}, want: nil},
		"non-ASCII digits":        {env: map[string]string{"CLAUDE_PID": "８９４"}, want: nil},
		"invalid UTF-8":           {env: map[string]string{"CLAUDE_PID": "89\xff4"}, want: nil},
		"embedded NUL":            {env: map[string]string{"CLAUDE_PID": "89\x004"}, want: nil},
		"other variables ignored": {env: map[string]string{"CLAUDE_CODE_PID": "1", "claude_pid": "2", "PPID": "3"}, want: nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := report.AnchorsFrom(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok })
			assert.Equal(t, tc.want, got)
		})
	}
}

// olderStranger finds a live process that is not in lineage and started no
// later than its first entry: what CLAUDE_PID names for a re-parented report.
// It returns 0 when the process table holds none, as in a minimal container.
func olderStranger(t *testing.T, lineage *report.Lineage) int {
	t.Helper()
	require.NotEmpty(t, lineage.Procs)
	in := map[int]bool{}
	for _, p := range lineage.Procs {
		in[p.PID] = true
	}
	entries, err := os.ReadDir("/proc")
	require.NoError(t, err)
	stat := report.ProcStat(os.DirFS("/proc"))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || in[pid] {
			continue
		}
		if s, err := stat(pid); err == nil && s.Start <= lineage.Procs[0].Start {
			return pid
		}
	}
	return 0
}

// stat builds a /proc/<pid>/stat line: pid, comm in parentheses, then the
// fields from state (field 3) on.
func stat(pid int, comm, rest string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(strconv.Itoa(pid) + " (" + comm + ") " + rest)}
}

// fields builds the fields after comm with the given parent (field 4) and
// start time (field 22), padding the 17 fields between them.
func fields(ppid, start string) string {
	return "S " + ppid + " 1 1 0 -1 4194304 1 0 0 0 1 2 0 0 20 0 1 0 " + start + " 1234 56 18446744073709551615\n"
}

// TestProcStatClasses walks what /proc/<pid>/stat can hold. The comm field is
// the executable name in parentheses and may itself contain spaces and
// parentheses, so fields are counted from the LAST ')': the parent is field
// 4 and the start time field 22 (proc(5)).
func TestProcStatClasses(t *testing.T) {
	t.Parallel()

	proc := fstest.MapFS{
		"100/stat": stat(100, "bash", fields("99", "8123")),
		"101/stat": stat(101, "a) b (c", fields("42", "7")),
		"102/stat": stat(102, "x) S 7 8", fields("43", "7")),
		"103/stat": stat(103, "two words", fields("44", "7")),
		"104/stat": stat(104, "new\nline", fields("45", "7")),
		"105/stat": stat(105, "ünï", fields("46", "7")),
		"106/stat": stat(106, "bad\xffutf8", fields("47", "7")),
		"107/stat": stat(107, "init", fields("0", "0")),
		"108/stat": stat(108, "big", fields("48", "18446744073709551615")),
		"109/stat": stat(109, "crlf", "S 49 1 1 0 -1 4194304 1 0 0 0 1 2 0 0 20 0 1 0 77\r\n"),
		"110/stat": stat(110, "nonl", "S 50 1 1 0 -1 4194304 1 0 0 0 1 2 0 0 20 0 1 0 78"),

		"200/stat": {Data: []byte("")},
		"201/stat": {Data: []byte("201 bash S 99")},
		"202/stat": stat(202, "bash", ""),
		"203/stat": stat(203, "bash", "S"),
		"204/stat": stat(204, "bash", fields("notapid", "7")),
		"205/stat": stat(205, "bash", fields("-1", "7")),
		"206/stat": stat(206, "bash", fields("99999999999999999999999", "7")),
		"207/stat": stat(207, "bash", fields("1.5", "7")),
		"208/stat": {Data: []byte("\x00\x00\x00")},
		"209/stat": stat(209, "bash", "S 99 1 1 0"),
		"210/stat": stat(210, "bash", fields("99", "soon")),
		"211/stat": stat(211, "bash", fields("99", "-5")),
		"212/stat": stat(212, "bash", fields("99", "18446744073709551616")),
		"213/stat": stat(213, "bash", "S 99 1 1 0 -1 4194304 1 0 0 0 1 2 0 0 20 0 1 0"),
	}

	tests := map[string]struct {
		pid     int
		want    report.Stat
		wantErr bool
	}{
		"plain comm":                                   {pid: 100, want: report.Stat{PPID: 99, Start: 8123}},
		"comm with parentheses and spaces":             {pid: 101, want: report.Stat{PPID: 42, Start: 7}},
		"comm that looks like the fields after it":     {pid: 102, want: report.Stat{PPID: 43, Start: 7}},
		"comm with a space":                            {pid: 103, want: report.Stat{PPID: 44, Start: 7}},
		"comm with a newline":                          {pid: 104, want: report.Stat{PPID: 45, Start: 7}},
		"comm with non-ASCII":                          {pid: 105, want: report.Stat{PPID: 46, Start: 7}},
		"comm with invalid UTF-8":                      {pid: 106, want: report.Stat{PPID: 47, Start: 7}},
		"parent 0 and start 0":                         {pid: 107, want: report.Stat{PPID: 0, Start: 0}},
		"start at the uint64 maximum":                  {pid: 108, want: report.Stat{PPID: 48, Start: 18446744073709551615}},
		"CRLF after the last field, exactly 20 fields": {pid: 109, want: report.Stat{PPID: 49, Start: 77}},
		"no final newline":                             {pid: 110, want: report.Stat{PPID: 50, Start: 78}},

		"missing process":                        {pid: 999, wantErr: true},
		"empty file":                             {pid: 200, wantErr: true},
		"no comm parentheses":                    {pid: 201, wantErr: true},
		"nothing after comm":                     {pid: 202, wantErr: true},
		"state but no parent":                    {pid: 203, wantErr: true},
		"parent not a number":                    {pid: 204, wantErr: true},
		"negative parent":                        {pid: 205, wantErr: true},
		"parent overflows an int":                {pid: 206, wantErr: true},
		"fractional parent":                      {pid: 207, wantErr: true},
		"binary garbage":                         {pid: 208, wantErr: true},
		"no start time field":                    {pid: 209, wantErr: true},
		"start time not a number":                {pid: 210, wantErr: true},
		"negative start time":                    {pid: 211, wantErr: true},
		"start time past uint64":                 {pid: 212, wantErr: true},
		"19 fields, one short of the start time": {pid: 213, wantErr: true},
		"pid 0 names no directory":               {pid: 0, wantErr: true},
		"negative pid":                           {pid: -5, wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.ProcStat(proc)(tc.pid)
			if tc.wantErr {
				require.ErrorIs(t, err, report.ErrProcStat)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestProcStatUnreadableFile: a stat file the process may not read is an
// ErrProcStat like any other unreadable ancestor.
func TestProcStatUnreadableFile(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(dir+"/300", 0o700))
	require.NoError(t, os.WriteFile(dir+"/300/stat", []byte("300 (bash) "+fields("99", "7")), 0o000))

	_, err := report.ProcStat(os.DirFS(dir))(300)
	require.ErrorIs(t, err, report.ErrProcStat)
}

// TestSelfLineageReadsTheRealProcessTable runs against the real /proc: the
// lineage is this test binary, then its parent, and start times never
// increase towards init. Off Linux there is no /proc and the lineage is nil,
// which makes a report trust HERDR_PANE_ID as it did before lineage existed.
func TestSelfLineageReadsTheRealProcessTable(t *testing.T) {
	t.Parallel()

	got := report.SelfLineage(func(string) (string, bool) { return "", false })
	if runtime.GOOS != "linux" {
		assert.Nil(t, got)
		return
	}
	require.NotNil(t, got)
	require.NotEmpty(t, got.Procs)
	assert.Equal(t, os.Getpid(), got.Procs[0].PID)
	if ppid := os.Getppid(); ppid > 1 {
		require.GreaterOrEqual(t, len(got.Procs), 2)
		assert.Equal(t, ppid, got.Procs[1].PID)
	}
	for i := 1; i < len(got.Procs); i++ {
		assert.LessOrEqual(t, got.Procs[i].Start, got.Procs[i-1].Start, "an ancestor never starts after its child")
	}
}

// TestSelfLineageAdmitsCLAUDE_PID: on the real process table, the pid the
// environment names joins the lineage when it is an older process outside it.
func TestSelfLineageAdmitsCLAUDE_PID(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("no /proc to read a lineage from")
	}
	none := func(string) (string, bool) { return "", false }
	stranger := olderStranger(t, report.SelfLineage(none))
	if stranger == 0 {
		t.Skip("no older process outside this test's lineage")
	}

	got := report.SelfLineage(func(k string) (string, bool) {
		if k == report.ClaudePIDEnv {
			return strconv.Itoa(stranger), true
		}
		return "", false
	})
	require.NotEmpty(t, got.Procs)
	assert.Equal(t, stranger, got.Procs[len(got.Procs)-1].PID)
}

// TestProcCmdlineClasses walks /proc/<pid>/cmdline: NUL-separated arguments
// with a trailing NUL, a process that rewrote its arguments without one, a
// kernel thread or zombie with none, and a process that has gone.
func TestProcCmdlineClasses(t *testing.T) {
	t.Parallel()
	proc := fstest.MapFS{
		"40/cmdline": {Data: []byte("/home/u/.codex/bin/codex\x00app-server\x00--listen\x00unix://\x00")},
		"41/cmdline": {Data: []byte("codex: worker title")},
		"42/cmdline": {Data: []byte{}},
		"43/cmdline": {Data: []byte("sh\x00-c\x00\x00echo\x00")},
		"45/cmdline": {Data: []byte("\xff\xfe\x00app-server\x00")},
	}
	tests := map[int]struct {
		want []string
		err  error
	}{
		40: {want: []string{"/home/u/.codex/bin/codex", "app-server", "--listen", "unix://"}},
		41: {want: []string{"codex: worker title"}},
		42: {want: []string{}},
		43: {want: []string{"sh", "-c", "", "echo"}},
		44: {err: report.ErrProcCmdline},
		45: {want: []string{"\xff\xfe", "app-server"}},
	}
	for pid, tc := range tests {
		t.Run(strconv.Itoa(pid), func(t *testing.T) {
			t.Parallel()
			got, err := report.ProcCmdline(proc)(pid)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSelfLineageChecksTheRealCodexHost: the real lineage carries a cmdline
// reader, so the Codex host check reads every ancestor rather than refusing
// unverified. Whether a Codex TUI or app-server is among them depends on who
// runs the tests, so any answer but "unverified" passes.
func TestSelfLineageChecksTheRealCodexHost(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("no /proc to read a lineage from")
	}
	err := report.SelfLineage(func(string) (string, bool) { return "", false }).CheckCodexHost()
	if err != nil && !errors.Is(err, report.ErrNoCodexHost) {
		require.ErrorIs(t, err, report.ErrCodexDaemon)
	}
}

// TestProcCmdlineUnreadableFile: a cmdline the process may not read is an
// ErrProcCmdline like one that has gone.
func TestProcCmdlineUnreadableFile(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(dir+"/300", 0o700))
	require.NoError(t, os.WriteFile(dir+"/300/cmdline", []byte("codex\x00"), 0o000))

	_, err := report.ProcCmdline(os.DirFS(dir))(300)
	require.ErrorIs(t, err, report.ErrProcCmdline)
}
