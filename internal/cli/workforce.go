package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/plugin"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// The workforce commands (ADR-001 D11, A23): team status, team up, worker
// spawn and worker retire. The daemon runs each; the command
// parses, asks and prints. On every path, a flag it does not know included, stdout is exactly
// one JSON object, the contract the herdr-workforce skill's wrapper reads:
// {"ok":true,"data":…} or {"ok":false,"error":{"code","message"}}. It exits 0
// when ok and 1 when refused.

// codeDaemonUnavailable answers a command whose daemon cannot be reached.
const codeDaemonUnavailable = "daemon_unavailable"

// codeDaemonNoAnswer answers a command whose daemon took the request and
// gave no answer: it may have run the op (A52).
const codeDaemonNoAnswer = "daemon_no_answer"

// errRefused is the error a refused workforce command returns, after
// printing its refusal, so the process exits 1.
var errRefused = errors.New("refused")

type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// printEnvelope writes e as one line of JSON.
func printEnvelope(w io.Writer, e envelope) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", raw)
	return err //nolint:wrapcheck // trivial write, message adds nothing
}

// refuseWith prints a refusal and returns errRefused, naming it.
func refuseWith(cmd *cobra.Command, code, message string) error {
	if err := printEnvelope(cmd.OutOrStdout(), envelope{Error: &envelopeError{Code: code, Message: message}}); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s: %s", errRefused, code, message)
}

// badRequest is a refusal for the command line itself.
func badRequest(cmd *cobra.Command, err error) error {
	return refuseWith(cmd, "bad_request", err.Error())
}

// workforceGroup is a group of workforce commands. Any word after it that
// names none of them is refused in JSON, and so is any flag error below it.
func workforceGroup(use, short string, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return badRequest(cmd, fmt.Errorf("%s needs a subcommand; see --help", cmd.CommandPath()))
			}
			return badRequest(cmd, fmt.Errorf("unknown command %q for %s", args[0], cmd.CommandPath()))
		},
	}
	c.SetFlagErrorFunc(badRequest)
	c.AddCommand(subs...)
	return c
}

func newTeamCmd() *cobra.Command {
	return workforceGroup("team", "Inspect and fill the boss's team (the herdr-workforce skill)", newTeamStatusCmd(), newTeamUpCmd())
}

func newWorkerCmd() *cobra.Command {
	return workforceGroup("worker", "Add and remove the team's workers (the herdr-workforce skill)", newWorkerSpawnCmd(), newWorkerRetireCmd())
}

// opFlags are the flags every workforce command takes.
type opFlags struct {
	stateDir string
	project  string
}

func (f *opFlags) add(c *cobra.Command, project bool) {
	c.Flags().StringVar(&f.stateDir, "state-dir", "", "plugin state directory (default: HERDR_PLUGIN_STATE_DIR, else herdr's state directory for this plugin)")
	if project {
		c.Flags().StringVar(&f.project, "project", "", "the team's project (default: the team whose workspace this pane is in, else the only tabs team)")
	}
}

// selector is the team the command means, as workforce.SelectorFor finds it
// by this command's lineage.
func (f *opFlags) selector(ctx context.Context) (workforce.Selector, error) {
	pane := report.PaneFrom(lookupEnvFrom(ctx))
	pane.Lineage = lineageFrom(ctx)()
	return workforce.SelectorFor(ctx, herdr.Client{SocketPath: pane.SocketPath}, f.project, pane) //nolint:wrapcheck // workforce wraps it
}

// runOp asks the daemon to run op with the request req builds once the
// daemon's socket is known, and prints its answer. req fails only when
// herdr does, finding the team (herdr_failed).
func (f *opFlags) runOp(cmd *cobra.Command, args []string, op string, req func() (any, error)) error {
	if len(args) > 0 {
		return badRequest(cmd, fmt.Errorf("%s takes no arguments, got %s", cmd.CommandPath(), strings.Join(args, " ")))
	}
	ctx := cmd.Context()
	lookup := lookupEnvFrom(ctx)
	env, err := plugin.EnvFrom(lookup)
	if err != nil {
		return refuseWith(cmd, codeDaemonUnavailable, fmt.Sprintf("read plugin environment: %v", err))
	}
	dir, derived := plugin.StateDir(f.stateDir, lookup)
	paths, err := daemon.PathsFor(dir, env.SocketPath)
	if err != nil {
		return refuseWith(cmd, codeDaemonUnavailable, noHerdr(err).Error())
	}
	r, err := req()
	if err != nil {
		return refuseWith(cmd, "herdr_failed", err.Error())
	}
	data, err := daemon.Workforce(ctx, paths.Socket, op, r)
	var refused *daemon.RequestError
	switch {
	case err == nil:
		return printEnvelope(cmd.OutOrStdout(), envelope{OK: true, Data: data})
	case errors.As(err, &refused):
		return refuseWith(cmd, refused.Code, refused.Message)
	case errors.Is(err, daemon.ErrUnavailable) && !errors.Is(err, daemon.ErrNotSent):
		return refuseWith(cmd, codeDaemonNoAnswer, "the daemon at "+paths.Socket+" took the request and gave no answer, so it may have done it: "+err.Error())
	case errors.Is(err, daemon.ErrUnavailable):
		msg := "no daemon answered at " + paths.Socket
		if derived {
			msg += " (herdr's state directory for this plugin, from XDG_STATE_HOME or HOME; pass --state-dir if herdr runs with another)"
		}
		return refuseWith(cmd, codeDaemonUnavailable, msg+"; run the daemon-restart action: "+err.Error())
	}
	return refuseWith(cmd, "failed", err.Error())
}

func newTeamStatusCmd() *cobra.Command {
	var f opFlags
	c := &cobra.Command{
		Use:   "status",
		Short: "Every worker's group, status, ctx, item, stage and idle time, and open slots",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return f.runOp(cmd, args, daemon.OpTeamStatus, func() (any, error) {
				sel, err := f.selector(cmd.Context())
				return workforce.StatusRequest{Selector: sel}, err
			})
		},
	}
	f.add(c, true)
	return c
}

func newTeamUpCmd() *cobra.Command {
	var f opFlags
	c := &cobra.Command{
		Use:   "up",
		Short: "Fill each group up to its default count, and open the team's dashboard",
		Long: "Adds workers to every group of the team until it has its configured\n" +
			"workers count, filling empty slots only, so a team already full gets\n" +
			"none; refused whole when the repo fails the preflight (tdaddy in a Go\n" +
			"repo). It opens the team's dashboard above the boss when none is open;\n" +
			"the dashboard never refuses the command. A worker that does not come up\n" +
			"refuses it with spawn_failed, naming the worker; the others stay, and\n" +
			"team status shows them.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return f.runOp(cmd, args, daemon.OpTeamUp, func() (any, error) {
				sel, err := f.selector(cmd.Context())
				return workforce.UpRequest{Selector: sel}, err
			})
		},
	}
	f.add(c, true)
	return c
}

func newWorkerSpawnCmd() *cobra.Command {
	var (
		f                              opFlags
		group, model, effort, taskFile string
		count                          int
	)
	c := &cobra.Command{
		Use:   "spawn",
		Short: "Add workers to a group, each in a tab of its own under Agentisan",
		Long: "Adds --count workers of the type --group, refused whole when they would\n" +
			"pass the group's slots or the repo fails the preflight (tdaddy in a Go\n" +
			"repo). Each gets the lowest free logical id, <short>-<type>-<n>, as its\n" +
			"tab label and agent name. With [routing] enabled, routing chooses\n" +
			"their model and effort from the type's role and --task-file (A50).",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			task, err := readTask(cmd.InOrStdin(), taskFile)
			if err != nil {
				return badRequest(cmd, fmt.Errorf("--task-file: %w", err))
			}
			return f.runOp(cmd, args, daemon.OpWorkerSpawn, func() (any, error) {
				sel, err := f.selector(cmd.Context())
				return workforce.SpawnRequest{Selector: sel, Group: group, Count: count, Model: model, Effort: effort, Task: task}, err
			})
		},
	}
	f.add(c, true)
	c.Flags().StringVar(&group, "group", "", "the worker type, a group of the project's configuration (required)")
	c.Flags().IntVar(&count, "count", 1, "how many workers to add")
	c.Flags().StringVar(&model, "model", "", "the model, instead of the group's; with routing on, a binding the role allows, by name or model id")
	c.Flags().StringVar(&effort, "effort", "", "the reasoning effort, such as high")
	c.Flags().StringVar(&taskFile, "task-file", "", "the task packet routing reads, as JSON: a file, or - for stdin")
	return c
}

// readTask reads --task-file's packet: none for "", stdin for "-".
func readTask(stdin io.Reader, path string) (*routing.Task, error) {
	switch path {
	case "":
		return nil, nil
	case "-":
		return routing.DecodeTask(stdin) //nolint:wrapcheck // routing's ErrInvalid says what is wrong
	}
	// #nosec G304 -- the path is the boss's own --task-file, read as data.
	f, err := os.Open(path)
	if err != nil {
		return nil, err //nolint:wrapcheck // the path error names the file
	}
	defer f.Close()              //nolint:errcheck // read-only
	return routing.DecodeTask(f) //nolint:wrapcheck // routing's ErrInvalid says what is wrong
}

func newWorkerRetireCmd() *cobra.Command {
	var (
		f            opFlags
		afterHandoff bool
	)
	c := &cobra.Command{
		Use:   "retire <id>",
		Short: "Close a worker's pane, by logical id or pane id",
		Long: "Closes the worker's pane and records it retired. With --after-handoff it\n" +
			"first waits until the worker has written its handoff; ask the worker to\n" +
			"write it before you run this.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return badRequest(cmd, fmt.Errorf("%s takes one logical id or pane id, got %d arguments", cmd.CommandPath(), len(args)))
			}
			return f.runOp(cmd, nil, daemon.OpWorkerRetire, func() (any, error) {
				return workforce.RetireRequest{Target: args[0], AfterHandoff: afterHandoff}, nil
			})
		},
	}
	f.add(c, false)
	c.Flags().BoolVar(&afterHandoff, "after-handoff", false, "close the pane only once the worker's handoff is written")
	return c
}
