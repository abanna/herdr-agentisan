// Package cli builds the product CLI: the `go-agents` command tree.
//
// Commands are thin. They parse flags, call the domain, and render — no
// business logic lives here, so the CLI and the REST API cannot disagree.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nerds-run/go-agents/internal/config"
	"github.com/nerds-run/go-agents/internal/notes"
)

// storeKey addresses the store stashed in the command context, so tests can
// inject one without a package-level variable.
type storeKey struct{}

// WithStore returns a context carrying s for the command tree to use.
func WithStore(ctx context.Context, s notes.Store) context.Context {
	return context.WithValue(ctx, storeKey{}, s)
}

// options holds flag values for ONE command tree. It is deliberately not a
// package-level var: cobra binds flags to the address it is given, so a global
// would be shared by every Root() in the process and two trees built in the
// same test binary would overwrite each other's --store.
type options struct {
	storePath string
}

// storeFrom returns the store injected for tests, or a file-backed store.
// The CLI runs one process per command, so an in-memory default would lose
// every note the moment the command returned.
func (o *options) storeFrom(ctx context.Context) (notes.Store, error) {
	if s, ok := ctx.Value(storeKey{}).(notes.Store); ok && s != nil {
		return s, nil
	}
	path := o.storePath
	if path == "" {
		if env := os.Getenv("GO_AGENTS_STORE"); env != "" {
			path = env
		} else {
			p, err := notes.DefaultPath()
			if err != nil {
				return nil, fmt.Errorf("locate default note store: %w", err)
			}
			path = p
		}
	}
	return notes.NewFileStore(path), nil
}

// Root builds the `go-agents` command tree.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "go-agents",
		Short: "Work with notes",
		Long: "go-agents is the product CLI. It drives the same domain the REST\n" +
			"API serves, so behaviour cannot drift between the two surfaces.",
		SilenceUsage: true,
		// Cobra prints the error itself; returning it as well double-prints.
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (%s)", config.Version, config.Commit),
	}
	opts := &options{}
	root.PersistentFlags().StringVar(&opts.storePath, "store", "",
		"path to the note store (default $GO_AGENTS_STORE, else the user config dir)")
	root.AddCommand(newNotesCmd(opts))
	return root
}

func newNotesCmd(o *options) *cobra.Command {
	c := &cobra.Command{
		Use:   "notes",
		Short: "Create, list, read and delete notes",
	}
	c.AddCommand(newNotesAddCmd(o), newNotesListCmd(o), newNotesGetCmd(o), newNotesRemoveCmd(o))
	return c
}

// asJSON is shared by every subcommand so `--json` behaves identically.
func addJSONFlag(c *cobra.Command, target *bool) {
	c.Flags().BoolVar(target, "json", false, "emit JSON instead of a table")
}

func render(w io.Writer, asJSON bool, v any, table func(io.Writer) error) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
		return nil
	}
	return table(w)
}

func newNotesAddCmd(o *options) *cobra.Command {
	var body string
	var asJSON bool

	c := &cobra.Command{
		Use:   "add <title>",
		Short: "Create a note",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := o.storeFrom(cmd.Context())
			if err != nil {
				return err
			}
			n, err := store.Create(cmd.Context(), notes.Draft{Title: args[0], Body: body})
			if err != nil {
				return fmt.Errorf("add note: %w", err)
			}
			return render(cmd.OutOrStdout(), asJSON, n, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "created %s\n", n.ID)
				return err //nolint:wrapcheck // trivial write, message adds nothing
			})
		},
	}
	c.Flags().StringVarP(&body, "body", "b", "", "note body")
	addJSONFlag(c, &asJSON)
	return c
}

func newNotesListCmd(o *options) *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "list",
		Short: "List every note, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := o.storeFrom(cmd.Context())
			if err != nil {
				return err
			}
			out, err := store.List(cmd.Context())
			if err != nil {
				return fmt.Errorf("list notes: %w", err)
			}
			return render(cmd.OutOrStdout(), asJSON, out, func(w io.Writer) error {
				if len(out) == 0 {
					_, err := fmt.Fprintln(w, "no notes")
					return err //nolint:wrapcheck // trivial write
				}
				tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
				if _, err := fmt.Fprintln(tw, "ID\tCREATED\tTITLE"); err != nil {
					return err //nolint:wrapcheck // trivial write
				}
				for _, n := range out {
					if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n",
						n.ID, n.CreatedAt.Format("2006-01-02 15:04:05"), n.Title); err != nil {
						return err //nolint:wrapcheck // trivial write
					}
				}
				if err := tw.Flush(); err != nil {
					return fmt.Errorf("flush table: %w", err)
				}
				return nil
			})
		},
	}
	addJSONFlag(c, &asJSON)
	return c
}

func newNotesGetCmd(o *options) *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "get <id>",
		Short: "Fetch one note by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := o.storeFrom(cmd.Context())
			if err != nil {
				return err
			}
			n, err := store.Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("get note: %w", err)
			}
			return render(cmd.OutOrStdout(), asJSON, n, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "%s\n\n%s\n", n.Title, n.Body)
				return err //nolint:wrapcheck // trivial write
			})
		},
	}
	addJSONFlag(c, &asJSON)
	return c
}

func newNotesRemoveCmd(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Delete one note by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := o.storeFrom(cmd.Context())
			if err != nil {
				return err
			}
			if err := store.Delete(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("delete note: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return err //nolint:wrapcheck // trivial write
		},
	}
}
