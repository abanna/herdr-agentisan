package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/notice"
)

// newNoticesCmd is notices (ADR-001 A44): the boss mutes a worker's notices
// for a while, ends a mute, and lists the mutes in force. The daemon holds
// them; like the workforce commands, stdout is exactly one JSON object on
// every path, and it exits 1 when refused.
func newNoticesCmd() *cobra.Command {
	return workforceGroup("notices", "Mute a worker's notices to its boss for a while", newNoticesMuteCmd(), newNoticesUnmuteCmd(), newNoticesListCmd())
}

func newNoticesMuteCmd() *cobra.Command {
	var (
		f   opFlags
		dur time.Duration
	)
	c := &cobra.Command{
		Use:   "mute <worker>",
		Short: "Drop a worker's notices to its boss for a while, never its context ceiling",
		Long: "Drops every notice about the worker, named as its notices name it (its logical\n" +
			"id, else its agent's name, else its pane id), until the mute ends: the context\n" +
			"ceiling always goes. The daemon holds mutes in memory, so a restart ends them.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return badRequest(cmd, fmt.Errorf("%s takes one worker, got %d arguments", cmd.CommandPath(), len(args)))
			}
			return f.runOp(cmd, nil, daemon.OpNoticesMute, func() (any, error) {
				return daemon.MuteRequest{Worker: args[0], For: dur}, nil
			})
		},
	}
	f.add(c, false)
	c.Flags().DurationVar(&dur, "for", notice.DefaultMute, "how long, at most "+notice.MaxMute.String())
	return c
}

func newNoticesUnmuteCmd() *cobra.Command {
	var f opFlags
	c := &cobra.Command{
		Use:   "unmute <worker>",
		Short: "End a worker's mute",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return badRequest(cmd, fmt.Errorf("%s takes one worker, got %d arguments", cmd.CommandPath(), len(args)))
			}
			return f.runOp(cmd, nil, daemon.OpNoticesUnmute, func() (any, error) {
				return daemon.UnmuteRequest{Worker: args[0]}, nil
			})
		},
	}
	f.add(c, false)
	return c
}

func newNoticesListCmd() *cobra.Command {
	var f opFlags
	c := &cobra.Command{
		Use:   "list",
		Short: "Every mute in force, and when it ends",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return f.runOp(cmd, args, daemon.OpNoticesList, func() (any, error) {
				return struct{}{}, nil
			})
		},
	}
	f.add(c, false)
	return c
}
