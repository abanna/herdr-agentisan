package settings

import (
	"os"
	"path/filepath"
	"slices"
)

// rotationKey is the table a profile, or a group of one, sets its
// thresholds in (A33).
const rotationKey = "rotation"

// Thresholds are one team's rotation thresholds (ADR-001 D7, A33): the
// context percentages, soft, hard and the ceiling, at which the daemon tells
// its boss a worker is due to rotate.
type Thresholds struct {
	// Project is the project's: the boss's, and every group's that sets
	// none of its own.
	Project []int
	// Groups are the groups that set their own, by name.
	Groups map[string][]int
}

// For is group's thresholds: its own, else the project's. The boss, in no
// group, is "".
func (t Thresholds) For(group string) []int {
	if ts, ok := t.Groups[group]; ok {
		return ts
	}
	return t.Project
}

// RotationSettings are every project's thresholds, and the thresholds of a
// team whose project the file does not name, such as the ◆ spaces with no
// [spaces] project.
type RotationSettings struct {
	Default  Thresholds
	Projects map[string]Thresholds
	// Unread holds why a project's repo file was not read, by project: its
	// repo does not resolve, or the file does not load or check. That
	// project has the thresholds its other layers set.
	Unread map[string]error
}

// For is project's thresholds.
func (r RotationSettings) For(project string) Thresholds {
	if t, ok := r.Projects[project]; ok {
		return t
	}
	return r.Default
}

// Rotations reads every project's thresholds, as the daemon does once when
// it starts (A19, A33). Each merges as the profile does, from the defaults,
// the shared tables, the project's own table and, unlike the notices
// settings, its repo file, which may set them (A46). Only a repo is resolved:
// no D12 check runs, so a repo without the Agentisan bundle still has its
// thresholds. A shared file that does not resolve is an error; a repo file
// that cannot be read is in Unread. No ConfigDir, or no file in it, is the
// defaults for every team and not an error. opts.Project and opts.Set are
// not read.
func Rotations(opts LoadOptions) (RotationSettings, error) {
	out := RotationSettings{Default: thresholdsOf(merge(defaults())), Projects: map[string]Thresholds{}, Unread: map[string]error{}}
	if opts.ConfigDir == "" {
		return out, nil
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if opts.Stat == nil {
		opts.Stat = os.Stat
	}
	// Load lints the whole file, as Notices does; the merges below only
	// read what it has checked, but for each repo file.
	if _, err := Load(LoadOptions{ConfigDir: opts.ConfigDir, LookupEnv: func(string) (string, bool) { return "", false }}); err != nil {
		return RotationSettings{}, err
	}
	shared, projects, err := loadShared(filepath.Join(opts.ConfigDir, FileName))
	if err != nil {
		return RotationSettings{}, err
	}
	def := merge(defaults(), shared)
	if err := def.checkFields(); err != nil {
		return RotationSettings{}, err
	}
	out.Default = thresholdsOf(def)
	for name, p := range projects {
		layers := []*source{defaults(), shared, p}
		view := merge(layers...)
		// Checked again as read here: the file can change after Load's
		// lint, and nothing unchecked is ever used.
		if err := view.checkFields(); err != nil {
			return RotationSettings{}, err
		}
		out.Projects[name] = thresholdsOf(view)
		repo, err := view.repo(opts.LookupEnv, opts.Stat)
		if err != nil {
			out.Unread[name] = err
			continue
		}
		repoFile, err := loadRepoFile(filepath.Join(repo, RepoFileName))
		if err != nil {
			out.Unread[name] = err
			continue
		}
		m := merge(slices.Concat(layers, []*source{repoFile})...)
		if err := m.checkFields(); err != nil {
			out.Unread[name] = err
			continue
		}
		out.Projects[name] = thresholdsOf(m)
	}
	return out, nil
}

// thresholdsOf reads the thresholds from a checked merge.
func thresholdsOf(m merged) Thresholds {
	p := buildProfile("", m.values)
	t := Thresholds{Project: p.Rotation.Thresholds}
	for name, g := range p.Groups {
		if g.Rotation.Thresholds != nil {
			if t.Groups == nil {
				t.Groups = map[string][]int{}
			}
			t.Groups[name] = g.Rotation.Thresholds
		}
	}
	return t
}
