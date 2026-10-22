package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/codexhooks"
	"github.com/abanna/herdr-agentisan/internal/config"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/plugin"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// daemonHooksKey addresses the daemon process hooks stashed in the command
// context. Tests inject them so a command never execs, signals or reads a
// real process.
type daemonHooksKey struct{}

// WithDaemonHooks returns a context whose daemon commands use h instead of
// exec'ing, signalling and reading real processes.
func WithDaemonHooks(ctx context.Context, h daemon.Hooks) context.Context {
	return context.WithValue(ctx, daemonHooksKey{}, h)
}

func newDaemonCmd() *cobra.Command {
	var stateDir string
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Start, stop and inspect the agentisan daemon",
		Long: "One daemon runs per herdr server. herdr's startup hook runs\n" +
			"`daemon start`; the daemon-restart action covers linking and manual\n" +
			"recovery, because linking fires no startup hook.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("%s needs a subcommand; see --help", cmd.CommandPath())
		},
	}
	c.PersistentFlags().StringVar(&stateDir, "state-dir", "",
		"plugin state directory (default: HERDR_PLUGIN_STATE_DIR; for health and stop, else herdr's state directory for this plugin)")
	// Only health and stop, which read or signal a running daemon, take
	// herdr's directory when neither is set (ADR-001 A51): a daemon started
	// under a wrong guess would be a second one for this herdr server.
	opts := func(cmd *cobra.Command) (daemon.Options, error) { return daemonOptions(cmd, stateDir, false) }
	clientOpts := func(cmd *cobra.Command) (daemon.Options, error) { return daemonOptions(cmd, stateDir, true) }
	c.AddCommand(
		newDaemonStartCmd(opts), newDaemonRunCmd(opts), newDaemonStopCmd(clientOpts),
		newDaemonRestartCmd(opts), newDaemonHealthCmd(clientOpts),
	)
	return c
}

// daemonOptions assembles the daemon's options from the herdr plugin
// environment, the loaded configuration and the --state-dir flag. A client's
// state directory falls back to herdr's own for this plugin.
func daemonOptions(cmd *cobra.Command, stateDirFlag string, client bool) (daemon.Options, error) {
	ctx := cmd.Context()
	lookup := lookupEnvFrom(ctx)
	env, err := plugin.EnvFrom(lookup)
	if err != nil {
		return daemon.Options{}, fmt.Errorf("read plugin environment: %w", err)
	}
	stateDir := cmp.Or(stateDirFlag, env.StateDir)
	if client {
		stateDir, _ = plugin.StateDir(stateDirFlag, lookup)
	}
	if stateDir == "" {
		return daemon.Options{}, fmt.Errorf("HERDR_PLUGIN_STATE_DIR is not set; run this from herdr or pass --state-dir: %w", daemon.ErrNoStateDir)
	}
	cfg, ok := config.From(ctx)
	if !ok {
		if cfg, err = config.Load(); err != nil {
			return daemon.Options{}, fmt.Errorf("load config: %w", err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return daemon.Options{}, fmt.Errorf("find this binary: %w", err)
	}
	hooks, _ := ctx.Value(daemonHooksKey{}).(daemon.Hooks)
	if hooks.Spawn == nil {
		hooks.Spawn = daemon.ExecSpawn(exe, "daemon", "run", "--state-dir", stateDir)
	}
	return daemon.Options{
		StateDir:        stateDir,
		HerdrSocket:     env.SocketPath,
		Herdr:           herdr.Client{SocketPath: env.SocketPath},
		LockWait:        cfg.DaemonLockWait,
		AllowUnverified: cfg.AllowUnverified,
		Version:         config.Version,
		Commit:          config.Commit,
		Logger:          logging.From(ctx),
		Hooks:           hooks,
		Team: &daemon.TeamOptions{
			ConfigDir: env.ConfigDir, GitHub: daemon.GitHubOptions{Gh: findGh(lookup)},
			PluginRoot: debugger.Root(lookup, exe), Binary: exe,
			Dashboard:  workforce.DashboardPane{PluginID: plugin.ID, Entrypoint: plugin.DashboardPane, Title: plugin.DashboardTitle},
			CodexHooks: func() (bool, error) { return codexhooks.InstalledIn(lookup) },
		},
	}, nil
}

// findGh is gh on the PATH the command was given, or "" (A29).
func findGh(lookup func(string) (string, bool)) string {
	path, _ := lookup("PATH")
	return github.Find(path)
}

// noHerdr explains a missing herdr socket in the terms a user can act on.
func noHerdr(err error) error {
	if errors.Is(err, daemon.ErrHerdrGone) {
		return fmt.Errorf("no herdr server: HERDR_SOCKET_PATH is unset or not a live socket; run this from herdr: %w", err)
	}
	return err
}

func printf(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err //nolint:wrapcheck // trivial write
}

// startDaemon starts the daemon and says what Start found (A48). A daemon
// that exited before it answered health is an error, so the command exits
// non-zero and herdr's plugin log records the start as failed; one still
// starting when Start stops waiting is not.
func startDaemon(cmd *cobra.Command, o daemon.Options) error {
	res, err := daemon.Start(cmd.Context(), o)
	logPath := "daemon.log"
	if p, perr := daemon.PathsFor(o.StateDir, o.HerdrSocket); perr == nil {
		logPath = p.Log
	}
	switch {
	case errors.Is(err, daemon.ErrDaemonExited):
		return fmt.Errorf("daemon start: %w; see %s", err, logPath)
	case err != nil:
		return fmt.Errorf("daemon start: %w", noHerdr(err))
	}
	return renderStart(cmd.OutOrStdout(), res, logPath)
}

func renderStart(w io.Writer, res daemon.StartResult, logPath string) error {
	switch {
	case res.AlreadyRunning && res.Healthy:
		return printf(w, "daemon already running (pid %d)\n", res.PID)
	case res.AlreadyRunning:
		return printf(w, "daemon already running (pid %d) but still starting, not answering health yet; see %s\n", res.PID, logPath)
	case res.Healthy:
		return printf(w, "daemon running (pid %d)\n", res.PID)
	default:
		return printf(w, "daemon starting (pid %d) but not answering health yet; see %s\n", res.PID, logPath)
	}
}

func newDaemonStartCmd(opts func(*cobra.Command) (daemon.Options, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the daemon unless one already runs for this herdr server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := opts(cmd)
			if err != nil {
				return err
			}
			return startDaemon(cmd, o)
		},
	}
}

func newDaemonRunCmd(opts func(*cobra.Command) (daemon.Options, error)) *cobra.Command {
	return &cobra.Command{
		Use:    "run",
		Short:  "Run the daemon in the foreground (what `daemon start` launches)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := opts(cmd)
			if err != nil {
				return err
			}
			paths, err := daemon.PathsFor(o.StateDir, o.HerdrSocket)
			if err != nil {
				return fmt.Errorf("daemon run: %w", noHerdr(err))
			}
			if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
				return fmt.Errorf("daemon run: create %s: %w", paths.Dir, err)
			}
			w, err := daemon.NewLogWriter(paths.Log, daemon.LogCap)
			if err != nil {
				return fmt.Errorf("daemon run: %w", err)
			}
			defer w.Close() //nolint:errcheck // the daemon is exiting; nothing is left to log the failure to
			cfg, _ := config.From(cmd.Context())
			level := cfg.LogLevel
			if level == "" {
				level = "info"
			}
			logger, err := logging.New(w, level, "json")
			if err != nil {
				return fmt.Errorf("daemon run: %w", err)
			}
			o.Logger = logger
			// Deferred after the log's Close, so it runs first: the flush can
			// still log, and runs once Run has released the lock (A21).
			events, flush := daemon.SetupEvents(cmd.Context(), o.Team.ConfigDir, telemetry.Config{
				ServiceName: cfg.ServiceName, Version: config.Version, Commit: config.Commit,
				Env: cfg.Env, OTLPEndpoint: cfg.OTLPEndpoint,
			}, logger)
			defer flush()
			o.Team.Events = events

			err = daemon.Run(cmd.Context(), o)
			switch {
			case errors.Is(err, daemon.ErrAlreadyRunning):
				logger.Info().Err(err).Msg("daemon already running; this one exits")
				return nil
			case err != nil:
				logger.Error().Err(err).Msg("daemon exited")
				return fmt.Errorf("daemon run: %w", err)
			}
			return nil
		},
	}
}

func newDaemonStopCmd(opts func(*cobra.Command) (daemon.Options, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := opts(cmd)
			if err != nil {
				return err
			}
			return stopDaemon(cmd, o)
		},
	}
}

// stopDaemon stops the daemon; one that is not running is already stopped.
func stopDaemon(cmd *cobra.Command, o daemon.Options) error {
	err := daemon.Stop(cmd.Context(), o)
	switch {
	case errors.Is(err, daemon.ErrHerdrGone):
		return fmt.Errorf("daemon stop: %w", noHerdr(err))
	case errors.Is(err, daemon.ErrNotRunning):
		return printf(cmd.OutOrStdout(), "no daemon running\n")
	case err != nil:
		return fmt.Errorf("daemon stop: %w", err)
	}
	return printf(cmd.OutOrStdout(), "daemon stopped\n")
}

func newDaemonRestartCmd(opts func(*cobra.Command) (daemon.Options, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Stop the daemon if it runs, then start it (the daemon-restart action)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := opts(cmd)
			if err != nil {
				return err
			}
			if err := stopDaemon(cmd, o); err != nil {
				return err
			}
			return startDaemon(cmd, o)
		},
	}
}

func newDaemonHealthCmd(opts func(*cobra.Command) (daemon.Options, error)) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "health",
		Short: "Ask the running daemon for its health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := opts(cmd)
			if err != nil {
				return err
			}
			paths, err := daemon.PathsFor(o.StateDir, o.HerdrSocket)
			if err != nil {
				return fmt.Errorf("daemon health: %w", noHerdr(err))
			}
			info, err := daemon.Health(cmd.Context(), paths.Socket)
			if errors.Is(err, daemon.ErrUnavailable) {
				return fmt.Errorf("no daemon answering at %s: %w", paths.Socket, err)
			}
			if err != nil {
				return fmt.Errorf("daemon health: %w", err)
			}
			return render(cmd.OutOrStdout(), asJSON, info, func(w io.Writer) error {
				return printf(w, "daemon pid %d, up %ds, %s (%s), herdr protocol %d at %s, state schema %d, %d focus rows\n",
					info.PID, info.UptimeSeconds, info.Version, info.Commit, info.HerdrProtocol, info.HerdrSocket,
					info.StoreVersion, info.FocusRows)
			})
		},
	}
	addJSONFlag(c, &asJSON)
	return c
}
