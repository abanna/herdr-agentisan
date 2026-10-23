package daemon_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestSnapshotCarriesTheBoardUnread (A60): each poll gives the team's
// snapshot the user's unread count on its board, and leaves it out at none;
// the boss reading moves only its own mark, and the user's seen clears it.
// A team whose project reports nothing else gets a project holding the count
// alone. No post is in the snapshot.
func TestSnapshotCarriesTheBoardUnread(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		config func(*testing.T) string
		bare   bool
	}{
		"a coloured project":       {config: projectConfig},
		"a project with no colour": {config: bareProjectConfig, bare: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			th.layout(fullTeam())
			dir := shortDir(t)
			o := teamOpts(t, dir, th, tc.config(t))
			o.Now = func() time.Time { return t0 }
			serving(t, o, th.fakeHerdr)
			sock := socketPath(t, dir, th.path)
			base := served(t, sock, anySnapshot)
			require.Equal(t, tc.bare, base.Project == nil, "the project reports a state of its own unless bare")
			if base.Project != nil {
				assert.Zero(t, base.Project.BoardUnread, "none unread: the key is left out")
			}

			// A second connection, as a person's sqlite3 would open one: the
			// daemon keeps its own.
			st, err := store.Open(t.Context(), paths(t, dir, th.fakeHerdr).DB)
			require.NoError(t, err)
			t.Cleanup(func() { _ = st.Close() })
			boardPost(t, st, "agentisan", "a body never served")
			last := boardPost(t, st, "agentisan", "two")
			boardPost(t, st, "web", "another team's")
			unread := func(n int) func(snapshot.Snapshot) bool {
				return func(s snapshot.Snapshot) bool { return s.Project != nil && s.Project.BoardUnread == n }
			}
			want := base
			want.Project = &snapshot.Project{}
			if base.Project != nil {
				p := *base.Project
				want.Project = &p
			}
			want.Project.BoardUnread = 2
			assert.Equal(t, want, served(t, sock, unread(2)))
			raw, err := json.Marshal(rawSnapshot(t, sock))
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "a body never served", "no post is in a snapshot")

			_, err = st.ReadBoard(t.Context(), "agentisan", store.ReaderBoss, 10)
			require.NoError(t, err)
			_, err = st.MarkSeen(t.Context(), "agentisan", store.ReaderUser, last.Seq-1)
			require.NoError(t, err)
			served(t, sock, unread(1))
			_, err = st.MarkSeen(t.Context(), "agentisan", store.ReaderUser, last.Seq)
			require.NoError(t, err)
			assert.Equal(t, base, served(t, sock, func(s snapshot.Snapshot) bool { return s.Project == nil || s.Project.BoardUnread == 0 }), "all seen: as before any post")
		})
	}
}

// bareProjectConfig writes a config dir naming project agentisan with no
// colour, so its snapshot reports no project state of its own.
func bareProjectConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := "schema_version = 1\n[projects.agentisan]\nrepo = \"~/Development/agentisan\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte(body), 0o600))
	return dir
}
