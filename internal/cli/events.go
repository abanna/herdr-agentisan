package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/trail"
)

// newEventsCmd is the debugger's window on the workflow event trail
// (ADR-001 item 12). No flag chooses the URL, a variable or a file: the client
// reads ELASTIC_URL and ELASTIC_API_KEY from the env file the shared
// config's [debugger] env_file names, at each query.
func newEventsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "events",
		Short: "Read the workflow event trail in Elasticsearch",
		Long: "events reads the daemon's workflow events back out of Elasticsearch (docs/debugger.md).\n" +
			"It reads ELASTIC_URL and ELASTIC_API_KEY, at each query, from the env file that\n" +
			"[debugger] env_file names in the shared config ($" + trail.ConfigDirEnv + ").",
	}
	c.AddCommand(newEventsWorkerCmd(), newEventsQueryCmd())
	return c
}

// trailClient builds the client from the config dir in the environment.
func trailClient(cmd *cobra.Command) (*trail.Client, error) {
	lookup := lookupEnvFrom(cmd.Context())
	dir, _ := lookup(trail.ConfigDirEnv)
	c, err := trail.FromConfig(cmd.Context(), dir, lookup, trail.EnvFileTimeout)
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	return c, nil
}

// workerTrail is `events worker`'s answer.
type workerTrail struct {
	ID      string         `json:"id"`
	Since   string         `json:"since"`
	Changes []trail.Change `json:"changes"`
	// Cut is whether the answer stopped at trail.RowLimit: older changes in
	// the window may be missing, never newer ones.
	Cut     bool `json:"cut,omitempty"`
	Partial bool `json:"partial,omitempty"`
}

func newEventsWorkerCmd() *cobra.Command {
	var since time.Duration
	var asJSON bool
	c := &cobra.Command{
		Use:   "worker <id>",
		Short: "Every status change of one worker or pane, oldest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := trail.WorkerQuery(args[0], since)
			if err != nil {
				return fmt.Errorf("events worker: %w", err)
			}
			client, err := trailClient(cmd)
			if err != nil {
				return err
			}
			table, err := client.Query(cmd.Context(), q)
			if err != nil {
				return fmt.Errorf("events worker: %w", err)
			}
			changes, cut, err := trail.WorkerChanges(table)
			if err != nil {
				return fmt.Errorf("events worker: %w", err)
			}
			out := workerTrail{ID: args[0], Since: window(since), Changes: changes, Cut: cut, Partial: table.Partial}
			return render(cmd.OutOrStdout(), asJSON, out, func(w io.Writer) error { return printWorkerTrail(w, out) })
		},
	}
	c.Flags().DurationVar(&since, "since", time.Hour, "how far back, whole minutes from 1m to 168h")
	addJSONFlag(c, &asJSON)
	return c
}

// window renders a whole-minute duration as hours or minutes.
func window(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

func printWorkerTrail(w io.Writer, t workerTrail) error {
	// Each value goes through cell, as events query's do, so none can break
	// its line or forge another: the id may hold a tab or a line break.
	var b strings.Builder
	id := cell(t.ID)
	switch len(t.Changes) {
	case 0:
		fmt.Fprintf(&b, "%s, the last %s: no status changes\n", id, t.Since)
	case 1:
		fmt.Fprintf(&b, "%s, the last %s: 1 status change\n", id, t.Since)
	default:
		fmt.Fprintf(&b, "%s, the last %s: %d status changes\n", id, t.Since, len(t.Changes))
	}
	for _, c := range t.Changes {
		who := cell(c.WorkerID)
		if c.PaneID != "" && c.PaneID != c.WorkerID {
			who += " (" + cell(c.PaneID) + ")"
		}
		group, bound := cell(c.Group), "unbound"
		if group == "" {
			group = "-"
		}
		if c.Bound {
			bound = "bound"
		}
		fmt.Fprintf(&b, "%s  %s  %s → %s  %s  %s\n", cell(c.At), who, cell(c.From), cell(c.To), group, bound)
	}
	if t.Cut {
		// The newest are kept, so a narrower --since never reaches the older.
		fmt.Fprintf(&b, "cut: only the newest %d are shown; read older ones with events query and a WHERE on @timestamp\n", trail.RowLimit)
	}
	if t.Partial {
		b.WriteString("partial: Elasticsearch did not search everything it was asked to\n")
	}
	_, err := io.WriteString(w, b.String())
	return err //nolint:wrapcheck // the command's own output stream
}

func newEventsQueryCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "query <esql>",
		Short: "Run an ES|QL read of the trail; it starts FROM " + trail.Source,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := trail.CheckQuery(args[0]); err != nil {
				return fmt.Errorf("events query: %w", err)
			}
			client, err := trailClient(cmd)
			if err != nil {
				return err
			}
			table, err := client.Query(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("events query: %w", err)
			}
			return render(cmd.OutOrStdout(), asJSON, table, func(w io.Writer) error { return printTable(w, table) })
		},
	}
	addJSONFlag(c, &asJSON)
	return c
}

// printTable renders an answer as aligned columns, a value per cell.
func printTable(w io.Writer, t trail.Table) error {
	var b strings.Builder
	names := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		names[i] = c.Name
	}
	b.WriteString(strings.Join(names, "\t") + "\n")
	for _, row := range t.Values {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = cell(v)
		}
		b.WriteString(strings.Join(cells, "\t") + "\n")
	}
	fmt.Fprintf(&b, "(%d rows)\n", len(t.Values))
	if t.Partial {
		b.WriteString("partial: Elasticsearch did not search everything it was asked to\n")
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := io.WriteString(tw, b.String()); err != nil {
		return err //nolint:wrapcheck // the command's own output stream
	}
	return tw.Flush() //nolint:wrapcheck // the command's own output stream
}

// cell renders one value: nil as empty, anything else as itself, with tabs
// and line breaks flattened so a value cannot break the table.
func cell(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		s = x
	case json.Number:
		s = x.String()
	default:
		s = fmt.Sprint(x)
	}
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}
