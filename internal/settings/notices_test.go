package settings_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// defaultNotices is every team's notices with nothing set: told everything,
// one message every 30 s at most, a repeat dropped for 5 min (A44), typed
// (A74).
var defaultNotices = settings.NoticeConfig{Gap: 30 * time.Second, Repeat: 5 * time.Minute, Delivery: settings.DeliveryTyped, Attention: attentionDefaults}

// TestNoticesForTheDaemon: the daemon reads every project's notices once,
// from the shared file alone. They merge as the profile does, so a shared
// [notices] applies to every project unless a
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
				Default: settings.NoticeConfig{Skip: []string{"turn_ended"}, Gap: time.Minute, Repeat: 5 * time.Minute, Delivery: settings.DeliveryTyped, Attention: attentionDefaults},
				Projects: map[string]settings.NoticeConfig{
					"app": {Skip: []string{"turn_ended"}, Gap: time.Minute, Repeat: 5 * time.Minute, Delivery: settings.DeliveryTyped, Attention: attentionDefaults},
					"web": {Skip: []string{}, Gap: time.Minute, Delivery: settings.DeliveryTyped, Attention: attentionDefaults},
				},
			},
		},
		"every kind, and the bounds": {
			file: "[projects.app]\nrepo = \"/x\"\nshort = \"a\"\n[projects.app.notices]\nskip = [\"turn_ended\", \"blocked\", \"unknown\", \"gone\", \"started\", \"ctx_soft\", \"ctx_hard\", \"merged\", \"ci_failed\", \"ci_passed\", \"dirty\", \"approved\"]\ngap = 3600\nrepeat = 86400\n[projects.web]\nrepo = \"/y\"\nshort = \"w\"\nnotices = { gap = 0, repeat = 0 }\n",
			want: settings.NoticeSettings{Default: defaultNotices, Projects: map[string]settings.NoticeConfig{
				"app": {Skip: []string{"turn_ended", "blocked", "unknown", "gone", "started", "ctx_soft", "ctx_hard", "merged", "ci_failed", "ci_passed", "dirty", "approved"}, Gap: time.Hour, Repeat: 24 * time.Hour, Delivery: settings.DeliveryTyped, Attention: attentionDefaults},
				"web": {Delivery: settings.DeliveryTyped, Attention: attentionDefaults},
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
		"a bad project value": {file: "[projects.app]\nrepo = \"/x\"\nshort = \"ap\"\n[projects.app.notices]\nrepeat = -5\n", wantErr: settings.ErrInvalid},
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
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[notices]\ngap = 10\n[projects.app]\nrepo = \"REPO\"\nshort = \"ap\"\n[projects.app.notices]\nskip = [\"unknown\"]\n", repo)

	got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", Set: []string{"notices.repeat=60"}, LookupEnv: envOf(nil)})
	require.NoError(t, err)
	assert.Equal(t, settings.LayerShared, got.Sources["notices.gap"])
	assert.Equal(t, settings.LayerProject, got.Sources["notices.skip"])
	assert.Equal(t, settings.LayerFlag, got.Sources["notices.repeat"])

	_, err = settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", Set: []string{"notices.gap=-1"}, LookupEnv: envOf(nil)})
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "notices.gap")

	writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n[notices]\ngap = 3600\n", repo)
	_, err = settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
	require.ErrorIs(t, err, settings.ErrUnknownKey, "a repo file cannot set notices: the daemon reads no repo file")
	assert.Contains(t, err.Error(), "notices")
}

// TestNoticesDeliveryDefaultsToTyped: with no notices.delivery anywhere,
// every team is typed, as before the board (A74).
func TestNoticesDeliveryDefaultsToTyped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n[notices]\ngap = 10\n[projects.app]\nrepo = \"/x\"\nshort = \"a\"\n"), 0o600))
	got, err := settings.Notices(dir)
	require.NoError(t, err)
	assert.Equal(t, settings.DeliveryTyped, got.Default.Delivery, "the default")
	assert.Equal(t, settings.DeliveryTyped, got.Projects["app"].Delivery, "a project that sets none")
}

// TestNoticesDeliveryPerProject: a project's own [projects.<p>.notices]
// sets its delivery over the shared table's, and every other team keeps
// the shared value (A74).
func TestNoticesDeliveryPerProject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n[notices]\ndelivery = \"typed\"\n[projects.app]\nrepo = \"/x\"\nshort = \"a\"\n[projects.app.notices]\ndelivery = \"board\"\n[projects.web]\nrepo = \"/y\"\nshort = \"w\"\n"), 0o600))
	got, err := settings.Notices(dir)
	require.NoError(t, err)
	assert.Equal(t, settings.DeliveryBoard, got.Projects["app"].Delivery, "the project's own table")
	assert.Equal(t, settings.DeliveryTyped, got.Projects["web"].Delivery, "a project with no table of its own")
	assert.Equal(t, settings.DeliveryTyped, got.Default.Delivery, "the default")
}

// TestNoticesDeliveryRefusesOtherValues: notices.delivery is typed or
// board, exactly; anything else is refused, naming the key. Each value is
// as the TOML file spells it.
func TestNoticesDeliveryRefusesOtherValues(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]string{
		"Board": `"Board"`, "empty": `""`, "post": `"post"`, "a space": `" typed"`, "a newline": `"board\n"`,
		"option-like": `"--board"`, "full-width": `"\uff42\uff4f\uff41\uff52\uff44"`, "a number": `1`, "a list": `["board"]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n[projects.app]\nrepo = \"/x\"\nshort = \"a\"\n[projects.app.notices]\ndelivery = "+v+"\n"), 0o600))
			_, err := settings.Notices(dir)
			require.ErrorIs(t, err, settings.ErrInvalid)
			assert.Contains(t, err.Error(), "notices.delivery")
		})
	}
}

// TestRepoFileCannotSetNoticesDelivery: a repo file is committed, and the
// daemon reads none, so notices.delivery in one is refused as an unknown
// key (A46), like every notices key.
func TestRepoFileCannotSetNoticesDelivery(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\nshort = \"ap\"\n", repo)
	writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n[notices]\ndelivery = \"board\"\n", repo)
	_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
	require.ErrorIs(t, err, settings.ErrUnknownKey)
	assert.Contains(t, err.Error(), "notices")
}

// attentionDefaults are [notices.attention]'s defaults (A74, the user's
// decisions 106c51d8): reminders at 10, 15, 25, 45 and 65 minutes, then
// three an hour, until someone reacts; 30 minutes while the boss is busy; a
// ceiling after 2 minutes.
var attentionDefaults = settings.AttentionConfig{After: 10 * time.Minute, CeilingAfter: 2 * time.Minute, Every: 5 * time.Minute, MaxEvery: 20 * time.Minute, Alerts: 0, BusyGrace: 30 * time.Minute, Sound: "request"}

// writeNotices writes a shared file holding app's project table and toml.
func writeNotices(t *testing.T, toml string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n[projects.app]\nrepo = \"/x\"\nshort = \"a\"\n"+toml), 0o600))
	return dir
}

// TestAttentionDefaults: with no [notices.attention], every team gets the
// seven decided values.
func TestAttentionDefaults(t *testing.T) {
	t.Parallel()
	got, err := settings.Notices(writeNotices(t, ""))
	require.NoError(t, err)
	assert.Equal(t, attentionDefaults, got.Default.Attention, "the default")
	assert.Equal(t, attentionDefaults, got.Projects["app"].Attention, "a project that sets none")
	none, err := settings.Notices("")
	require.NoError(t, err)
	assert.Equal(t, attentionDefaults, none.Default.Attention, "no configuration at all")
}

// TestAttentionBounds: each key just outside its bounds is refused, naming
// it, in the shared table and in a project's; each bound itself is taken.
func TestAttentionBounds(t *testing.T) {
	t.Parallel()
	outside := map[string]string{
		"after 59": "after = 59", "after -1": "after = -1", "after 86401": "after = 86401",
		"ceiling_after 29": "ceiling_after = 29", "ceiling_after 3601": "ceiling_after = 3601",
		"every 59": "every = 59", "every 3601": "every = 3601",
		"max_every below every": "every = 600\nmax_every = 599", "max_every 14401": "max_every = 14401",
		"alerts -1": "alerts = -1", "alerts 101": "alerts = 101",
		"busy_grace -1": "busy_grace = -1", "busy_grace 86401": "busy_grace = 86401",
		"sound Request": `sound = "Request"`, "sound empty": `sound = ""`, "sound bell": `sound = "bell"`,
		"sound a space": `sound = " request"`, "sound a newline": `sound = "request\n"`,
		"sound full-width": `sound = "\uff52\uff45\uff51\uff55\uff45\uff53\uff54"`, "sound a number": `sound = 1`,
		"after a string": `after = "600"`, "alerts a float": `alerts = 1.5`,
	}
	for name, kv := range outside {
		key := "notices.attention." + strings.Fields(kv)[0]
		if name == "max_every below every" {
			key = "notices.attention.max_every"
		}
		for table, toml := range map[string]string{"shared": "[notices.attention]\n" + kv + "\n", "project": "[projects.app.notices.attention]\n" + kv + "\n"} {
			t.Run(name+", "+table, func(t *testing.T) {
				t.Parallel()
				_, err := settings.Notices(writeNotices(t, toml))
				require.ErrorIs(t, err, settings.ErrInvalid)
				// The refused key itself, as "<key> (<layer>, <file>)": a
				// mention in another key's refusal, such as max_every's
				// "below notices.attention.every", does not count.
				assert.Contains(t, err.Error(), key+" (")
			})
		}
	}
	inside := []struct {
		toml string
		want func(*settings.AttentionConfig)
	}{
		{"after = 0", func(a *settings.AttentionConfig) { a.After = 0 * time.Second }},
		{"after = 60", func(a *settings.AttentionConfig) { a.After = 60 * time.Second }},
		{"after = 86400", func(a *settings.AttentionConfig) { a.After = 86400 * time.Second }},
		{"ceiling_after = 30", func(a *settings.AttentionConfig) { a.CeilingAfter = 30 * time.Second }},
		{"ceiling_after = 3600", func(a *settings.AttentionConfig) { a.CeilingAfter = 3600 * time.Second }},
		{"every = 60", func(a *settings.AttentionConfig) { a.Every = 60 * time.Second }},
		{"every = 1200", func(a *settings.AttentionConfig) { a.Every = 1200 * time.Second }},
		{"every = 3600\nmax_every = 3600", func(a *settings.AttentionConfig) { a.Every, a.MaxEvery = 3600*time.Second, 3600*time.Second }},
		{"max_every = 300", func(a *settings.AttentionConfig) { a.MaxEvery = 300 * time.Second }},
		{"max_every = 14400", func(a *settings.AttentionConfig) { a.MaxEvery = 14400 * time.Second }},
		{"alerts = 100", func(a *settings.AttentionConfig) { a.Alerts = 100 }},
		{"busy_grace = 0", func(a *settings.AttentionConfig) { a.BusyGrace = 0 * time.Second }},
		{"busy_grace = 86400", func(a *settings.AttentionConfig) { a.BusyGrace = 86400 * time.Second }},
		{`sound = "none"`, func(a *settings.AttentionConfig) { a.Sound = "none" }},
		{`sound = "done"`, func(a *settings.AttentionConfig) { a.Sound = "done" }},
	}
	for _, c := range inside {
		t.Run(c.toml, func(t *testing.T) {
			t.Parallel()
			got, err := settings.Notices(writeNotices(t, "[projects.app.notices.attention]\n"+c.toml+"\n"))
			require.NoError(t, err)
			want := attentionDefaults
			c.want(&want)
			assert.Equal(t, want, got.Projects["app"].Attention)
		})
	}
}

// TestAttentionPerProject: a project's own table sets its keys over the
// shared table's, key by key; every other team keeps the shared values.
func TestAttentionPerProject(t *testing.T) {
	t.Parallel()
	got, err := settings.Notices(writeNotices(t, "[projects.web]\nrepo = \"/y\"\nshort = \"w\"\n[notices.attention]\nalerts = 5\n[projects.app.notices.attention]\nafter = 0\n"))
	require.NoError(t, err)
	off, shared := attentionDefaults, attentionDefaults
	off.After, off.Alerts, shared.Alerts = 0, 5, 5
	assert.Equal(t, off, got.Projects["app"].Attention, "the project's own table, off")
	assert.Equal(t, shared, got.Projects["web"].Attention, "a project with no table of its own")
	assert.Equal(t, shared, got.Default.Attention, "the default")
}

// TestRepoFileCannotSetAttention: a repo file is committed, and the daemon
// reads none, so [notices.attention] in one is refused as an unknown key
// (A46), like every notices key.
func TestRepoFileCannotSetAttention(t *testing.T) {
	t.Parallel()
	repo, dir := newRepo(t), t.TempDir()
	writeFile(t, dir, settings.FileName, "schema_version = 1\n[projects.app]\nrepo = \"REPO\"\nshort = \"ap\"\n", repo)
	writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n[notices.attention]\nafter = 0\n", repo)
	_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: "app", LookupEnv: envOf(nil)})
	require.ErrorIs(t, err, settings.ErrUnknownKey)
	assert.Contains(t, err.Error(), "notices")
}
