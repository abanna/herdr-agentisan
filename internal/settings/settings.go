// Package settings is the versioned herdr-agentisan.toml contract (ADR-001
// D9): project profiles, the layers they are resolved from, the provenance of
// every resolved value, secret references, and the D12 checks a profile must
// pass before anything is launched from it.
//
// It is not internal/config. That package is the process's own runtime
// configuration (log level, OTLP endpoint, build provenance), read from the
// environment once at startup. This one is the user's file-backed workflow
// configuration, read from HERDR_PLUGIN_CONFIG_DIR and a project's repo, and
// it carries a schema_version because the daemon and the boss both depend on
// its shape.
package settings

import (
	"errors"
	"io/fs"
	"regexp"
)

// SchemaVersion is the only schema_version this binary reads.
const SchemaVersion = 1

// FileName is the shared config file in the plugin config directory
// (HERDR_PLUGIN_CONFIG_DIR). RepoFileName is a project's own overlay, at the
// root of its repo.
const (
	FileName     = "herdr-agentisan.toml"
	RepoFileName = ".herdr-agentisan.toml"
)

// BundleMarker is the file, relative to a project's repo, that proves the
// Agentisan bundle is installed there (D12).
const BundleMarker = ".claude/version.txt"

// MaxFileBytes caps the shared file and a repo file. A config file is a few
// kilobytes; the cap stops a runaway or hostile file (a symlink to
// /dev/zero) from growing the read without limit.
const MaxFileBytes = 1 << 20

// Layer names, lowest precedence first. Resolved.Sources maps every leaf path
// to one of them.
const (
	LayerDefault = "default"
	LayerShared  = "shared"
	LayerProject = "project"
	LayerRepo    = "repo"
	LayerFlag    = "flag"
)

// Sentinel errors. Callers branch on these with errors.Is; every message names
// the file or layer and the key path, and never a value that could be a
// secret.
var (
	// ErrUnsupportedSchema means schema_version is missing or is not
	// SchemaVersion, in the shared file or a repo file.
	ErrUnsupportedSchema = errors.New("unsupported schema_version")
	// ErrUnknownKey means a layer sets a key the schema does not know.
	ErrUnknownKey = errors.New("unknown config key")
	// ErrSecretValue means a secret field holds something other than an
	// { env = "NAME" } or { op = "op://..." } reference, such as the secret
	// itself.
	ErrSecretValue = errors.New("secret is not a reference")
	// ErrBypassesAgentisan means the profile would launch an agent outside
	// Agentisan (D12): not claude or codex, --bare, project settings left
	// out, or no Agentisan bundle in the repo.
	ErrBypassesAgentisan = errors.New("agent command bypasses Agentisan")
	// ErrInvalid means a value breaks a field rule (type, colour, GitHub
	// repo, thresholds, groups, names, repo path) or a file does not parse.
	ErrInvalid = errors.New("invalid config")
	// ErrNoProject means the requested project is not in the shared file.
	ErrNoProject = errors.New("no such project")
	// ErrNoConfigDir means no plugin config directory was given.
	ErrNoConfigDir = errors.New("no plugin config directory")
	// ErrSecretUnavailable means a secret reference resolved to nothing: the
	// variable is unset or empty, or 1Password failed or returned nothing.
	ErrSecretUnavailable = errors.New("secret unavailable")
)

// Profile is one project's resolved configuration.
type Profile struct {
	// Name is the profile's key under [projects].
	Name string
	// Repo is the absolute path of the project's repo, after ~ expansion.
	Repo string
	// Color is the project's colour as #rrggbb; empty when unset.
	Color string
	// GitHub is the project's repo as owner/name; empty when unset.
	GitHub string
	// Agent is how sessions are launched.
	Agent Agent
	// Groups are the worker groups, keyed by group name.
	Groups map[string]Group
	// Verify holds the verification commands, each an argv list.
	Verify [][]string
	// Integrations are external services, keyed by name.
	Integrations map[string]Integration
	// Rotation holds the context thresholds that drive rotation notices.
	Rotation Rotation
}

// Agent is the command every session starts with, and its model.
type Agent struct {
	Command []string
	Model   string
}

// Group is a worker group: how many workers team up starts, and the cap the
// boss can scale to.
type Group struct {
	Workers int
	Slots   int
}

// Integration is an external service. Token is nil when unset.
type Integration struct {
	Token *SecretRef
	URL   string
}

// Rotation holds the three ascending context percentages at which the boss is
// notified.
type Rotation struct {
	Thresholds []int
}

// Resolved is the outcome of Load.
type Resolved struct {
	// Project is the selected project's name; empty when none was selected.
	Project string
	// Profile is the selected project's profile; nil when none was selected.
	Profile *Profile
	// Values is the merged tree: tables are map[string]any, leaves are
	// string, int, []string, []int, [][]string or SecretRef.
	Values map[string]any
	// Sources maps every leaf path in Values to the layer that set it.
	Sources map[string]string
	// Projects lists every project in the shared file, sorted.
	Projects []string
}

// Leaf is one resolved value and the layer it came from.
type Leaf struct {
	Path  string
	Value any
	Layer string
}

// LoadOptions says where to read configuration from and how.
type LoadOptions struct {
	// ConfigDir holds FileName; usually HERDR_PLUGIN_CONFIG_DIR. Required.
	ConfigDir string
	// Project selects a profile. Empty resolves defaults, the shared tables
	// and Set only, and lists the projects.
	Project string
	// Set holds --set overrides, each key.path=value with a TOML value.
	Set []string
	// LookupEnv reads HOME for ~ expansion; nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Stat checks the repo and its Agentisan bundle; nil means os.Stat.
	Stat func(string) (fs.FileInfo, error)
}

// namePattern is the alphabet of project, group and integration names: they
// become key path segments and herdr labels, so no dots, no spaces, no case.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
