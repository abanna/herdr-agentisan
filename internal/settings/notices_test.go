package settings_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// defaultNotices is every team's notices with nothing set: told everything,
// one message every 30 s at most, a repeat dropped for 5 min (A44).
var defaultNotices = settings.NoticeConfig{Gap: 30 * time.Second, Repeat: 5 * time.Minute}

// TestNoticesForTheDaemon: the daemon reads every project's notices once,
// from the shared file alone. They merge as the profile does, so a shared
// [notices] is every project's, and every unnamed team's, unless a
// project's own table says otherwise. A file that does not resolve is its
// error.
func TestNoticesForTheDaemon(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		file    string // "" for no file; "nodir" for no config dir
		want    settings.NoticeSettings
		wantErr error
	}{
		"no config dir": {file: "nodir", want: settings.NoticeSettings{Default: defaultNotices, Projects: map[string]settings.NoticeConfig{}}},
		"no file":       {want: settings.NoticeSettings{Default: defaultNotices, Projects: map[string]settings.NoticeConfig{}}},
		"defaults for a project that sets none": {
			file: "[projects.app]\nrepo = \"/x\"\nshort = \"a\"\n",
			want: settings.NoticeSettings{Default: defaultNotices, Projects: map[string]settings.NoticeConfig{"app": defaultNotices}},
		},
		"shared, then the project's own": {
			file: "[notices]\nskip = [\"turn_ended\"]\ngap = 60\n[projects.app]\nrepo = \"/x\"\nshort = \"a\"\n[projects.web]\nrepo = \"/y\"\nshort = \"w\"\n[projects.web.notices]\nskip = []\nrepeat = 0\n",
			want: settings.NoticeSettings{
				Default: settings.NoticeConfig{Skip: []string{"turn_ended"}, Gap: time.Minute, Repeat: 5 * time.Minute},
				Projects: map[string]settings.NoticeConfig{
					"app": {Skip: []string{"turn_ended"}, Gap: time.Minute, Repeat: 5 * time.Minute},
					"web": {Skip: []string{}, Gap: time.Minute},
				},
			},
		},
		"every kind, and the bounds": {
			file: "[projects.app]\nrepo = \"/x\"\nshort = \"a\"\n[projects.app.notices]\nskip = [\"turn_ended\", \"blocked\", \"unknown\", \"gone\", \"started\", \"ctx_soft\", \"ctx_hard\"]\ngap = 3600\nrepeat = 86400\n[projects.web]\nrepo = \"/y\"\nshort = \"w\"\nnotices = { gap = 0, repeat = 0 }\n",
			want: settings.NoticeSettings{Default: defaultNotices, Projects: map[string]settings.NoticeConfig{
				"app": {Skip: []string{"turn_ended", "blocked", "unknown", "gone", "started", "ctx_soft", "ctx_hard"}, Gap: time.Hour, Repeat: 24 * time.Hour},
				"web": {},
			}},
		},
		"an unknown kind":     {file: "[notices]\nskip = [\"ctx\"]\n", wantErr: settings.ErrInvalid},
		"the context ceiling": {file: "[notices]\nskip = [\"ctx_hard\", \"ctx_ceiling\"]\n", wantErr: settings.ErrInvalid},
		"a kind in capitals":  {file: "[notices]\nskip = [\"Blocked\"]\n", wantErr: settings.ErrInvalid},
		"an empty kind":       {file: "[notices]\nskip = [\"\"]\n", wantErr: settings.ErrInvalid},
		"a kind twice":        {file: "[notices]\nskip = [\"gone\", \"gone\"]\n", wantErr: settings.ErrInvalid},
		"a full-width kind":   {file: "[notices]\nskip = [\"\uff47\uff4f\uff4e\uff45\"]\n", wantErr: settings.ErrInvalid},
		"a kind with a space": {file: "[notices]\nskip = [\"gone \"]\n", wantErr: settings.ErrInvalid},
		"an option-like kind": {file: "[notices]\nskip = [\"--gone\"]\n", wantErr: settings.ErrInvalid},
		"a gap past int64":    {file: "[notices]\ngap = 9223372036854775808\n", wantErr: settings.ErrInvalid},
		"the largest int64":   {file: "[notices]\nrepeat = 9223372036854775807\n", wantErr: settings.ErrInvalid},
		"skip is no list":     {file: "[notices]\nskip = \"gone\"\n", wantErr: settings.ErrInvalid},
		"a negative gap":      {file: "[notices]\ngap = -1\n", wantErr: settings.ErrInvalid},
		"a gap past an hour":  {file: "[notices]\ngap = 3601\n", wantErr: settings.ErrInvalid},
		"a repeat past a day": {file: "[notices]\nrepeat = 86401\n", wantErr: settings.ErrInvalid},
		"a gap in words":      {file: "[notices]\ngap = \"30s\"\n", wantErr: settings.ErrInvalid},
		"a fractional gap":    {file: "[notices]\ngap = 1.5\n", wantErr: settings.ErrInvalid},
		"an unknown key":      {file: "[notices]\nmute = [\"qa\"]\n", wantErr: settings.ErrUnknownKey},
		"a bad project value": {file: "[projects.app]\nrepo = \"/x\"\n[projects.app.notices]\nrepeat = -5\n", wantErr: settings.ErrInvalid},
		"a broken file":       {file: "[notices\n", wantErr: settings.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			switch tc.file {
			case "nodir":
				dir = ""
			case "":
			default:
				require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n"+tc.file), 0o600))
			}
			got, err := settings.Notices(dir)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				if name != "a broken file" && name != "a gap past int64" {
					assert.Contains(t, err.Error(), "notices", "the error names the key")
				}
				if name == "the context ceiling" {
					assert.Contains(t, err.Error(), "always reaches the boss")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestNoticesResolveWithProvenance: config resolve shows the notices keys
// and the layer each came from, and --set reaches them.
func TestNoticesResolveWithProvenance(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[notices]\ngap = 10\n[projects.app]\nrepo = \"REPO\"\n[projects.app.notices]\nskip = [\"unknown\"]\n", repo)

	got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", Set: []string{"notices.repeat=60"}, LookupEnv: envOf(nil)})
	require.NoError(t, err)
	assert.Equal(t, settings.LayerShared, got.Sources["notices.gap"])
	assert.Equal(t, settings.LayerProject, got.Sources["notices.skip"])
	assert.Equal(t, settings.LayerFlag, got.Sources["notices.repeat"])

	_, err = settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", Set: []string{"notices.gap=-1"}, LookupEnv: envOf(nil)})
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "notices.gap")
}
