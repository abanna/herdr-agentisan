package preflight_test

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/preflight"
)

// TestKillGroup: a run's Cancel sends SIGKILL to tdaddy's whole process
// group. A group already gone is os.ErrProcessDone, which exec takes as
// done; any other failure keeps its cause and names the program.
func TestKillGroup(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kill error
		want error
		says string
	}{
		"killed":       {},
		"already gone": {kill: syscall.ESRCH, want: os.ErrProcessDone},
		"not permitted": {
			kill: syscall.EPERM, want: syscall.EPERM,
			says: "kill " + bin + "'s process group: operation not permitted",
		},
		"any other failure": {
			kill: syscall.EINVAL, want: syscall.EINVAL,
			says: "kill " + bin + "'s process group: invalid argument",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var pids []int
			var sigs []syscall.Signal
			err := preflight.KillGroup(func(pid int, sig syscall.Signal) error {
				pids, sigs = append(pids, pid), append(sigs, sig)
				return tc.kill
			}, 4242, bin)
			assert.Equal(t, []int{-4242}, pids, "one kill, of the whole group, not only its leader")
			assert.Equal(t, []syscall.Signal{syscall.SIGKILL}, sigs)
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
			if tc.says != "" {
				assert.Equal(t, tc.says, err.Error())
				assert.NotErrorIs(t, err, os.ErrProcessDone, "a group that may still run is not done")
			}
		})
	}
}
