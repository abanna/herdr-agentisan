package report_test

import (
	"math"
	"os"
	"runtime"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/report"
)

// TestProcStartedAtClasses: a Linux start, clock ticks since boot, is put
// on the wall clock as ps does it: /proc/stat's btime plus the ticks at
// UserHZ to the second (A47). A stat with no usable btime, or a start the
// clock cannot hold, has no wall-clock time.
func TestProcStartedAtClasses(t *testing.T) {
	t.Parallel()
	const btime = 1_792_000_000
	boot := time.Unix(btime, 0)
	stat := "cpu  1 2 3 4\ncpu0 1 2 3 4\nintr 9\nctxt 7\nbtime 1792000000\nprocesses 42\n"
	maxTicks := uint64(math.MaxInt64 / int64(time.Second/report.UserHZ))
	tests := map[string]struct {
		stat  string
		start uint64
		want  time.Time
		bad   bool
	}{
		"ticks since boot":            {stat: stat, start: 4200, want: boot.Add(42 * time.Second)},
		"one tick is 10 ms":           {stat: stat, start: 1, want: boot.Add(10 * time.Millisecond)},
		"at boot":                     {stat: stat, start: 0, want: boot},
		"the most ticks a time holds": {stat: stat, start: maxTicks, want: boot.Add(time.Duration(maxTicks) * 10 * time.Millisecond)},
		"one tick more":               {stat: stat, start: maxTicks + 1, bad: true},
		"the largest start":           {stat: stat, start: math.MaxUint64, bad: true},
		"btime last, no newline":      {stat: "cpu 1\nbtime 1792000000", start: 100, want: boot.Add(time.Second)},
		"btime first, tab-separated":  {stat: "btime\t1792000000\ncpu 1\n", start: 100, want: boot.Add(time.Second)},
		"btime ended by CRLF":         {stat: "cpu 1\r\nbtime 1792000000\r\n", start: 100, want: boot.Add(time.Second)},
		"no btime line":               {stat: "cpu 1\nprocesses 42\n", bad: true},
		"btimex is not btime":         {stat: "btimex 1792000000\n", bad: true},
		"btime with no value":         {stat: "btime\n", bad: true},
		"btime with two values":       {stat: "btime 1792000000 1\n", bad: true},
		"btime not a number":          {stat: "btime soon\n", bad: true},
		"btime negative":              {stat: "btime -1\n", bad: true},
		"btime zero":                  {stat: "btime 0\n", bad: true},
		"btime past int64":            {stat: "btime 9223372036854775808\n", bad: true},
		"an empty stat":               {stat: "", bad: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.ProcStartedAt(fstest.MapFS{"stat": {Data: []byte(tc.stat)}})(tc.start)
			if tc.bad {
				require.ErrorIs(t, err, report.ErrStartedAt)
				assert.True(t, got.IsZero())
				return
			}
			require.NoError(t, err)
			assert.True(t, tc.want.Equal(got), "got %v, want %v", got, tc.want)
		})
	}
}

// TestProcStartedAtReadsStatEachTime: the boot time is read at every call,
// not once, and a stat that cannot be read is no wall-clock time.
func TestProcStartedAtReadsStatEachTime(t *testing.T) {
	t.Parallel()
	proc := fstest.MapFS{"stat": {Data: []byte("btime 100\n")}}
	at := report.ProcStartedAt(proc)
	got, err := at(100)
	require.NoError(t, err)
	assert.True(t, time.Unix(101, 0).Equal(got))

	proc["stat"] = &fstest.MapFile{Data: []byte("btime 200\n")}
	got, err = at(100)
	require.NoError(t, err)
	assert.True(t, time.Unix(201, 0).Equal(got), "the new boot time")

	delete(proc, "stat")
	_, err = at(100)
	require.ErrorIs(t, err, report.ErrStartedAt)
}

// TestKinfoStartedAtClasses: a darwin start, as KinfoStat reads it, is
// already microseconds since the epoch.
func TestKinfoStartedAtClasses(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		start uint64
		want  time.Time
		bad   bool
	}{
		"the epoch":          {start: 0, want: time.Unix(0, 0)},
		"a start":            {start: 1_792_000_000_123_456, want: time.Unix(1_792_000_000, 123_456_000)},
		"the largest it can": {start: math.MaxInt64, want: time.UnixMicro(math.MaxInt64)},
		"past int64":         {start: math.MaxInt64 + 1, bad: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := report.KinfoStartedAt(tc.start)
			if tc.bad {
				require.ErrorIs(t, err, report.ErrStartedAt)
				return
			}
			require.NoError(t, err)
			assert.True(t, tc.want.Equal(got), "got %v, want %v", got, tc.want)
		})
	}
}

// TestSystemStartedAtCoversLinuxAndDarwin: on Linux and darwin the real
// converter puts this process's own start, as SystemStat reads it, on the
// wall clock: in the past, and recent. Anywhere else there is none.
func TestSystemStartedAtCoversLinuxAndDarwin(t *testing.T) {
	t.Parallel()
	at := report.SystemStartedAt()
	switch runtime.GOOS {
	case "linux", "darwin":
		require.NotNil(t, at)
		s, err := report.SystemStat()(os.Getpid())
		require.NoError(t, err)
		got, err := at(s.Start)
		require.NoError(t, err)
		now := time.Now()
		// btime is whole seconds, so the start can read up to 1 s late.
		assert.False(t, got.After(now.Add(time.Second)), "started %v, after now %v", got, now)
		assert.WithinDuration(t, now, got, time.Hour, "this test binary started moments ago")
	default:
		assert.Nil(t, at)
	}
}
