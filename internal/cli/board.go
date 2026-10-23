package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// ErrWaitTimedOut is board wait's error when its timeout passed with no
// post: the command exits 3, to be started again (ADR-001 A60).
var ErrWaitTimedOut = errors.New("no post came before the timeout")

// ExitCode is the exit status a command's error ends the process with: 3
// for a board wait that timed out, 1 for any other error, 0 for none.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrWaitTimedOut):
		return 3
	}
	return 1
}

// newBoardCmd is board (ADR-001 A60): the boss reads its team's board, and
// waits in the background for a post on it. The daemon tells who asks by
// the kernel, never by a flag: only a team's boss, or the user's dashboard
// in its boss's tab, reads; a worker is refused (not_allowed), since
// workers may only post. Like the workforce commands, stdout is exactly one
// JSON object on every path, and it exits 1 when refused.
func newBoardCmd() *cobra.Command {
	return workforceGroup("board", "Read the team's board, or wait for a post on it (the boss and the user only)", newBoardReadCmd(), newBoardWaitCmd())
}

func newBoardReadCmd() *cobra.Command {
	var (
		f     opFlags
		limit int
		all   bool
	)
	c := &cobra.Command{
		Use:   "read",
		Short: "The board's posts you have not read, oldest first, marking them read",
		Long: "Reads your team's board as its boss, or as the user from the dashboard in the\n" +
			"boss's tab: the posts after your read mark, oldest first, and moves the mark\n" +
			"past them, so each is read once; more says how many are left. With --all, the\n" +
			"newest posts instead, newest first, read or not, and no mark moves.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return f.runOp(cmd, args, daemon.OpBoardRead, func() (any, error) {
				return daemon.BoardReadRequest{Limit: limit, All: all}, nil
			})
		},
	}
	f.add(c, false)
	c.Flags().IntVar(&limit, "limit", daemon.DefaultBoardLimit, fmt.Sprintf("at most this many posts, 1 to %d", store.MaxBoardRead))
	c.Flags().BoolVar(&all, "all", false, "the newest posts, newest first, read or not, moving no mark")
	return c
}

func newBoardWaitCmd() *cobra.Command {
	var (
		f       opFlags
		after   int64
		timeout time.Duration
	)
	c := &cobra.Command{
		Use:   "wait",
		Short: "Wait for a post on the board; run it in the background",
		Long: "Answers once a post past --after is on your team's board, at once if one is\n" +
			"there already. --after is your read mark when not given, so a post made between\n" +
			"a read and the next wait is never missed. It exits 3 when --timeout passes with\n" +
			"none: start it again. A later wait from the same pane ends this one\n" +
			"(superseded), and a daemon that stops ends it (daemon_gone).",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			req := daemon.BoardWaitRequest{Timeout: timeout}
			if cmd.Flags().Changed("after") {
				req.After = &after
			}
			return f.runOpThen(cmd, args, daemon.OpBoardWait, func() (any, error) { return req, nil }, func(data json.RawMessage) error {
				var res daemon.BoardStateResult
				if err := json.Unmarshal(data, &res); err != nil {
					return fmt.Errorf("read the daemon's answer: %w", err)
				}
				if res.TimedOut {
					return fmt.Errorf("%w: %s", ErrWaitTimedOut, timeout)
				}
				return nil
			})
		},
	}
	f.add(c, false)
	c.Flags().Int64Var(&after, "after", 0, "answer once a post past this seq is on the board (default: your read mark)")
	c.Flags().DurationVar(&timeout, "timeout", daemon.DefaultBoardWait, "how long to wait, at most "+daemon.MaxBoardWait.String())
	return c
}
