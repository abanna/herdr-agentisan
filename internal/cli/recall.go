package cli

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/recall"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/trail"
)

// newRecallCmd is the boss's rule library (NERD-5741): the sections of the
// files its standing rules are kept in, read word for word, each with the
// file and lines it is written in. It reads the files and nothing else.
func newRecallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "recall",
		Short: "Read the boss's rules, word for word, with the file and lines they are in",
		Long: "recall reads the library that [recall] memory_dirs and bullet_files name in the shared\n" +
			"config ($" + trail.ConfigDirEnv + "): each memory file, and each top-level bullet of each bullet file, is\n" +
			"one section with an ID. `list` prints the sections; `show` prints the ones you name,\n" +
			"exactly as the file has them. The files are read at each call.",
	}
	c.AddCommand(newRecallListCmd(), newRecallShowCmd())
	return c
}

// recallSection is a section as `recall` prints it as JSON.
type recallSection struct {
	ID      string `json:"id"`
	File    string `json:"file"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Heading string `json:"heading"`
	Text    string `json:"text,omitempty"`
}

// recallLibrary cuts the library from the shared config in configDir, or in
// the environment's.
func recallLibrary(cmd *cobra.Command, name, configDir string) (recall.Library, error) {
	lookup := lookupEnvFrom(cmd.Context())
	if configDir == "" {
		configDir, _ = lookup(trail.ConfigDirEnv)
	}
	if configDir == "" {
		return recall.Library{}, errors.New(name + ": pass --config-dir or set " + trail.ConfigDirEnv)
	}
	src, err := settings.RecallSources(configDir, lookup)
	if err != nil {
		return recall.Library{}, fmt.Errorf("%s: %w", name, err)
	}
	lib, err := recall.Load(src)
	if err != nil {
		return recall.Library{}, fmt.Errorf("%s: %w", name, err)
	}
	return lib, nil
}

func recallSections(secs []recall.Section, withText bool) []recallSection {
	out := make([]recallSection, len(secs))
	for i, s := range secs {
		out[i] = recallSection{ID: s.ID, File: s.File, Start: s.Start, End: s.End, Heading: s.Heading}
		if withText {
			out[i].Text = s.Text
		}
	}
	return out
}

func newRecallListCmd() *cobra.Command {
	var configDir string
	var asJSON bool
	c := &cobra.Command{
		Use:   "list",
		Short: "Every section: its ID, where it is written and its heading",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, err := recallLibrary(cmd, "recall list", configDir)
			if err != nil {
				return err
			}
			return render(cmd.OutOrStdout(), asJSON, map[string]any{"sections": recallSections(lib.Sections, false)}, func(w io.Writer) error {
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				for _, s := range lib.Sections {
					if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", s.ID, s.Ref(), s.Heading); err != nil {
						return err //nolint:wrapcheck // the command's own output stream
					}
				}
				if err := tw.Flush(); err != nil {
					return err //nolint:wrapcheck // the command's own output stream
				}
				return printf(w, "\n%d sections\n", len(lib.Sections))
			})
		},
	}
	c.Flags().StringVar(&configDir, "config-dir", "", "the plugin's config dir (default: $"+trail.ConfigDirEnv+")")
	addJSONFlag(c, &asJSON)
	return c
}

func newRecallShowCmd() *cobra.Command {
	var configDir string
	var asJSON bool
	c := &cobra.Command{
		Use:   "show <id>...",
		Short: "The sections you name, word for word, each under its file and lines",
		Long: "show prints each section named, in the order asked, under a header with its ID, file and\n" +
			"lines, exactly as the file has them. An ID that is not in the library, because the section\n" +
			"was removed or its first line edited, fails the command and prints nothing.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, ids []string) error {
			lib, err := recallLibrary(cmd, "recall show", configDir)
			if err != nil {
				return err
			}
			secs, err := lib.Pick(ids)
			if err != nil {
				return fmt.Errorf("recall show: %w", err)
			}
			return render(cmd.OutOrStdout(), asJSON, map[string]any{"sections": recallSections(secs, true)}, func(w io.Writer) error {
				for i, s := range secs {
					sep := ""
					if i > 0 {
						sep = "\n"
					}
					if err := printf(w, "%s== %s  %s\n%s\n", sep, s.ID, s.Ref(), s.Text); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	c.Flags().StringVar(&configDir, "config-dir", "", "the plugin's config dir (default: $"+trail.ConfigDirEnv+")")
	addJSONFlag(c, &asJSON)
	return c
}
