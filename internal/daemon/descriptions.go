package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// groupDescriptions are every configured project's group descriptions, which
// the team poll re-reads whenever the shared file changes (ADR-001 A36): the
// one setting the daemon picks up without a restart, as v12 picked up an edit
// to its descriptions file. The teams themselves, their colours included,
// are still read once at start. Only the poll's goroutine uses it.
type groupDescriptions struct {
	// dir is the plugin config directory; "" reads nothing.
	dir string
	// looked is set once the file has been looked at.
	looked bool
	// seen is the file as last looked at; nil when it was missing.
	seen fs.FileInfo
	// statFailed is set while the file cannot be looked at, so that is
	// logged once.
	statFailed bool
	// byProject are the last good descriptions, by project, then group.
	byProject map[string]map[string]string
}

func newGroupDescriptions(dir string) *groupDescriptions { return &groupDescriptions{dir: dir} }

// refresh re-reads the descriptions when the shared file changed since it
// last looked: another modification time, size or file, as when an editor
// saves by renaming. It looks before it reads, so an edit between the two is
// read at the next poll.
//
// A file that does not resolve, or cannot be read, keeps the last good
// descriptions, logged once: it is read again after its next change. A file
// that is gone has none, as when there is none at start. A file that cannot
// be looked at keeps them, logged once until it can be.
func (d *groupDescriptions) refresh(log zerolog.Logger) {
	if d == nil || d.dir == "" {
		return
	}
	fi, err := os.Stat(filepath.Join(d.dir, settings.FileName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		d.statFailed = false
		if d.looked && d.seen == nil {
			return
		}
		d.looked, d.seen = true, nil
		if d.byProject != nil {
			log.Info().Msg("group descriptions: herdr-agentisan.toml is gone, so no group has one")
		}
		d.byProject = nil
		return
	case err != nil:
		if !d.statFailed {
			d.statFailed = true
			log.Warn().Err(err).Msg("group descriptions: herdr-agentisan.toml cannot be looked at; the last ones stay until it can")
		}
		return
	}
	d.statFailed = false
	if d.looked && d.seen != nil && unchanged(d.seen, fi) {
		return
	}
	d.looked, d.seen = true, fi
	teams, err := settings.Teams(d.dir)
	if err != nil {
		log.Warn().Err(err).Msg("group descriptions: herdr-agentisan.toml does not resolve; the last ones stay until it is fixed")
		return
	}
	d.byProject = map[string]map[string]string{}
	for _, t := range teams {
		if t.Descriptions != nil {
			d.byProject[t.Project] = t.Descriptions
		}
	}
	log.Info().Int("projects", len(d.byProject)).Msg("group descriptions read")
}

// unchanged reports whether b is the file a was: the same file, modified at
// the same time, of the same size.
func unchanged(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

// of is project's descriptions, by group; nil when it has none.
func (d *groupDescriptions) of(project string) map[string]string {
	if d == nil {
		return nil
	}
	return d.byProject[project]
}
