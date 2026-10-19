package settings_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestTelemetryEndpointPrecedence: [telemetry] endpoint is the collector the
// daemon sends its workflow events to (A21). Like [spaces] it is a shared
// key: the shared file sets it, --set overrides it, and its provenance is
// reported like any leaf. Unset, nothing is exported.
func TestTelemetryEndpointPrecedence(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\n"
	tests := map[string]struct {
		file    string // the shared file after schema_version; "-" for no file
		project string
		set     []string
		want    string
		layer   string // "" when no layer reports the key
	}{
		"no config file: none":                     {file: "-"},
		"no [telemetry] table: none":               {file: app},
		"an empty [telemetry] table: none":         {file: "[telemetry]\n" + app},
		"set in the shared file":                   {file: "[telemetry]\nendpoint = \"localhost:4318\"\n" + app, want: "localhost:4318", layer: settings.LayerShared},
		"set, a project selected":                  {file: "[telemetry]\nendpoint = \"localhost:4318\"\n" + app, project: "app", want: "localhost:4318", layer: settings.LayerShared},
		"set as a dotted key":                      {file: "telemetry.endpoint = \"127.0.0.1:4318\"\n" + app, want: "127.0.0.1:4318", layer: settings.LayerShared},
		"an IPv6 loopback":                         {file: "[telemetry]\nendpoint = \"[::1]:4318\"\n" + app, want: "[::1]:4318", layer: settings.LayerShared},
		"set empty, explicitly off":                {file: "[telemetry]\nendpoint = \"\"\n" + app, want: "", layer: settings.LayerShared},
		"--set beats the shared file":              {file: "[telemetry]\nendpoint = \"localhost:4318\"\n" + app, set: []string{"telemetry.endpoint=collector:4318"}, want: "collector:4318", layer: settings.LayerFlag},
		"--set with no key in the file":            {file: app, set: []string{"telemetry.endpoint=localhost:4318"}, want: "localhost:4318", layer: settings.LayerFlag},
		"--set empty turns a shared one off":       {file: "[telemetry]\nendpoint = \"localhost:4318\"\n" + app, set: []string{`telemetry.endpoint=""`}, want: "", layer: settings.LayerFlag},
		"a host name with dashes and the top port": {file: "[telemetry]\nendpoint = \"otel-collector.local:65535\"\n" + app, want: "otel-collector.local:65535", layer: settings.LayerShared},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			if tc.file != "-" {
				writeFile(t, dir, settings.FileName, "schema_version = 1\n"+tc.file, repo)
			}

			got, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: tc.project, Set: tc.set, LookupEnv: envOf(nil)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.TelemetryEndpoint())
			l, ok := leaf(t, got, "telemetry.endpoint")
			if tc.layer == "" {
				assert.False(t, ok, "no layer reports telemetry.endpoint")
				return
			}
			require.True(t, ok)
			assert.Equal(t, tc.want, l.Value)
			assert.Equal(t, tc.layer, l.Layer)
		})
	}
}

// TestTelemetryEndpointErrors: the endpoint is host:port, as
// HERDR_AGENTISAN_OTLP_ENDPOINT is, because the exporter builds the URL
// around it; a URL, a missing or out-of-range port, or anything that would
// bend that URL is refused at load, naming the key, its layer and file. Only
// the shared file and --set may set it: there is one daemon per herdr
// server, not one per project. No secret goes here yet; the token slot
// arrives with TLS (A21).
func TestTelemetryEndpointErrors(t *testing.T) {
	t.Parallel()

	const app = "[projects.app]\nrepo = \"REPO\"\n"
	bad := func(endpoint string) string { return "[telemetry]\nendpoint = \"" + endpoint + "\"\n" + app }
	tests := map[string]struct {
		file     string
		repoFile string
		project  string
		set      []string
		wantErr  error
		wantIn   []string
		wantOut  []string
	}{
		"a URL":                    {file: bad("http://localhost:4318"), wantErr: settings.ErrInvalid, wantIn: []string{"telemetry.endpoint", "shared", settings.FileName, "host:port"}},
		"no port":                  {file: bad("localhost"), wantErr: settings.ErrInvalid, wantIn: []string{"telemetry.endpoint"}},
		"no host":                  {file: bad(":4318"), wantErr: settings.ErrInvalid, wantIn: []string{"telemetry.endpoint"}},
		"port zero":                {file: bad("localhost:0"), wantErr: settings.ErrInvalid},
		"port past 65535":          {file: bad("localhost:65536"), wantErr: settings.ErrInvalid},
		"a signed port":            {file: bad("localhost:+1"), wantErr: settings.ErrInvalid},
		"a named port":             {file: bad("localhost:otlp"), wantErr: settings.ErrInvalid},
		"a negative port":          {file: bad("localhost:-1"), wantErr: settings.ErrInvalid},
		"a port past uint64":       {file: bad("localhost:99999999999999999999"), wantErr: settings.ErrInvalid},
		"a non-ASCII host":         {file: bad("kollektör:4318"), wantErr: settings.ErrInvalid},
		"a NUL in the host":        {file: bad("local\\u0000host:4318"), wantErr: settings.ErrInvalid},
		"a byte-order mark":        {file: bad("\\uFEFFlocalhost:4318"), wantErr: settings.ErrInvalid},
		"a trailing CR":            {file: bad("localhost:4318\\r"), wantErr: settings.ErrInvalid},
		"a path in the host":       {file: bad("a/b:4318"), wantErr: settings.ErrInvalid},
		"white space in the host":  {file: bad("local host:4318"), wantErr: settings.ErrInvalid},
		"user info in the host":    {file: bad("u:s3cret@localhost:4318"), wantErr: settings.ErrInvalid, wantOut: []string{"s3cret"}},
		"a URL from --set":         {file: app, set: []string{"telemetry.endpoint=https://x:443"}, wantErr: settings.ErrInvalid, wantIn: []string{"telemetry.endpoint", "--set"}},
		"not a string":             {file: "[telemetry]\nendpoint = 4318\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"telemetry.endpoint", "want a string"}},
		"telemetry is not a table": {file: "telemetry = \"localhost:4318\"\n" + app, wantErr: settings.ErrInvalid, wantIn: []string{"telemetry", "want a table"}},
		"an unknown key, a token among them": {
			file: "[telemetry]\ntoken = { env = \"ELASTIC_KEY\" }\n" + app, wantErr: settings.ErrUnknownKey, wantIn: []string{"telemetry.token"},
		},
		"in a project table": {
			file: app + "[projects.app.telemetry]\nendpoint = \"localhost:4318\"\n", wantErr: settings.ErrUnknownKey,
			wantIn: []string{"projects.app.telemetry"},
		},
		"in a repo file": {
			file: app, repoFile: "[telemetry]\nendpoint = \"localhost:4318\"\n", project: "app",
			wantErr: settings.ErrUnknownKey, wantIn: []string{"telemetry", settings.RepoFileName},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, dir := newRepo(t), t.TempDir()
			writeFile(t, dir, settings.FileName, "schema_version = 1\n"+tc.file, repo)
			if tc.repoFile != "" {
				writeFile(t, repo, settings.RepoFileName, "schema_version = 1\n"+tc.repoFile, repo)
			}

			_, err := settings.Load(settings.LoadOptions{ConfigDir: dir, Project: tc.project, Set: tc.set, LookupEnv: envOf(nil)})
			require.ErrorIs(t, err, tc.wantErr)
			for _, s := range tc.wantIn {
				assert.Contains(t, err.Error(), s)
			}
			for _, s := range tc.wantOut {
				assert.NotContains(t, err.Error(), s, "the value is never echoed: it may carry credentials")
			}
		})
	}
}

// TestTelemetryEndpointChecksTheOverride: the override is held to the key's
// rule, so the endpoint the daemon logs is always a bare host:port. A bad one
// is ErrInvalid naming the variable, never echoing the value, and the file
// is not consulted in its place.
func TestTelemetryEndpointChecksTheOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName),
		[]byte("schema_version = 1\n[telemetry]\nendpoint = \"localhost:4318\"\n"), 0o600))

	for name, override := range map[string]string{
		"a URL":              "http://localhost:4318",
		"credentials":        "user:s3cret@localhost:4318",
		"no port":            "collector",
		"a path":             "localhost:4318/v1/logs",
		"white space":        "localhost :4318",
		"port zero":          "localhost:0",
		"port past 65535":    "localhost:65536",
		"a negative port":    "localhost:-1",
		"a port past uint64": "localhost:99999999999999999999",
		"a non-ASCII host":   "kollektör:4318",
		"invalid UTF-8":      "local\xffhost:4318",
		"a NUL byte":         "local\x00host:4318",
		"a byte-order mark":  "\uFEFFlocalhost:4318",
		"a trailing CR":      "localhost:4318\r",
		"like a flag":        "--help",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := settings.TelemetryEndpoint(dir, override)
			require.ErrorIs(t, err, settings.ErrInvalid)
			assert.Empty(t, got, "the file's endpoint is not used in its place")
			assert.Contains(t, err.Error(), "HERDR_AGENTISAN_OTLP_ENDPOINT")
			assert.NotContains(t, err.Error(), override)
			assert.NotContains(t, err.Error(), "s3cret")
		})
	}
}

// TestTelemetryEndpointForTheDaemon: the daemon reads the endpoint once at
// start. HERDR_AGENTISAN_OTLP_ENDPOINT, passed as the override, wins without
// the file being read, so a broken file cannot stop an endpoint set in the
// environment. Otherwise no directory, or no file, is no endpoint and not an
// error; a file that does not resolve is an error the caller decides about.
func TestTelemetryEndpointForTheDaemon(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		file     string // "" for no file; "nodir" for no config dir
		override string
		want     string
		wantErr  error
	}{
		"no config dir":                       {file: "nodir"},
		"no file":                             {},
		"a config dir that does not exist":    {file: "missing"},
		"the key":                             {file: "[telemetry]\nendpoint = \"localhost:4318\"\n", want: "localhost:4318"},
		"no key":                              {file: "[spaces]\n"},
		"the override beats the key":          {file: "[telemetry]\nendpoint = \"localhost:4318\"\n", override: "collector:4318", want: "collector:4318"},
		"the override with no config dir":     {file: "nodir", override: "localhost:4318", want: "localhost:4318"},
		"the override, the file broken":       {file: "color = \"red\"\n", override: "localhost:4318", want: "localhost:4318"},
		"a bad endpoint":                      {file: "[telemetry]\nendpoint = \"http://x:1\"\n", wantErr: settings.ErrInvalid},
		"an unrelated bad value, no override": {file: "color = \"red\"\n", wantErr: settings.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			switch tc.file {
			case "nodir":
				dir = ""
			case "missing":
				dir = filepath.Join(dir, "gone")
			case "":
			default:
				require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n"+tc.file), 0o600))
			}

			got, err := settings.TelemetryEndpoint(dir, tc.override)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Equal(t, tc.want, got)
		})
	}
}
