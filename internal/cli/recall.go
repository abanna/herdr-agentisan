package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/recall"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/trail"
)

// newRecallCmd is the boss's rule library (NERD-5741), read from files only.
func newRecallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "recall",
		Short: "Read the boss's rules, word for word, with the file and lines they are in",
		Long: "recall reads the library [recall] names in the shared config ($" + trail.ConfigDirEnv + "): each memory file,\n" +
			"and each top-level bullet of each bullet file, is a section with an ID. Files are read at each call.",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "Every section: its ID, where it is written and its heading",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, err := recallLibrary(cmd)
			if err != nil {
				return fmt.Errorf("recall list: %w", err)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			for _, s := range lib.Sections {
				if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", s.ID, s.Ref(), s.Heading); err != nil {
					return err //nolint:wrapcheck // the command's own output stream
				}
			}
			if err := tw.Flush(); err != nil {
				return err //nolint:wrapcheck // the command's own output stream
			}
			return printf(cmd.OutOrStdout(), "\n%d sections\n", len(lib.Sections))
		},
	}
	show := &cobra.Command{
		Use:   "show <id>...",
		Short: "The sections you name, word for word, each under its file and lines",
		Long:  "show prints each section named, in order, exactly as its file has it. An ID that is gone fails the command and prints nothing.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, ids []string) error {
			lib, err := recallLibrary(cmd)
			if err != nil {
				return fmt.Errorf("recall show: %w", err)
			}
			secs, err := lib.Pick(ids)
			if err != nil {
				return fmt.Errorf("recall show: %w", err)
			}
			blocks := make([]string, len(secs))
			for i, s := range secs {
				blocks[i] = fmt.Sprintf("== %s  %s\n%s\n", s.ID, s.Ref(), s.Text)
			}
			return printf(cmd.OutOrStdout(), "%s", strings.Join(blocks, "\n"))
		},
	}
	c.AddCommand(list, show)
	return c
}

// recallLibrary cuts the library from the config dir's shared file.
func recallLibrary(cmd *cobra.Command) (recall.Library, error) {
	lookup := lookupEnvFrom(cmd.Context())
	dir, _ := lookup(trail.ConfigDirEnv)
	if dir == "" {
		return recall.Library{}, errors.New("set " + trail.ConfigDirEnv + ", the plugin's config dir")
	}
	src, err := settings.RecallSources(dir, lookup)
	if err != nil {
		return recall.Library{}, fmt.Errorf("the config: %w", err)
	}
	lib, err := recall.Load(src)
	if err != nil {
		return recall.Library{}, fmt.Errorf("the library: %w", err)
	}
	return lib, nil
}
