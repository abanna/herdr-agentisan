package settings

import (
	"maps"
	"path/filepath"
	"slices"
)

// Header is what the daemon reads of the configuration, once, to collect a
// project's header from GitHub (ADR-001 A29): the project, and its repo as
// owner/name.
type Header struct {
	Project string
	GitHub  string
}

// Headers reads, from the shared file in configDir, every project that has
// a github repo, by project name. The repo merges as the profile's does,
// from the defaults, the shared tables and the project's own table; the
// project's repo file is not read, so, as for the colour (ReadSpaces), a
// github set only there is not seen. Like ReadSpaces it selects no project,
// so it runs no repo or D12 check and reads no environment variable. No
// configDir, or no file in it, is no project and not an error.
func Headers(configDir string) ([]Header, error) {
	if configDir == "" {
		return nil, nil
	}
	// Load lints the whole file, as ReadSpaces does; the merge below only
	// reads what it has checked.
	if _, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }}); err != nil {
		return nil, err
	}
	shared, projects, err := loadShared(filepath.Join(configDir, FileName))
	if err != nil {
		return nil, err
	}
	var out []Header
	for _, name := range slices.Sorted(maps.Keys(projects)) {
		v, _ := merge(defaults(), shared, projects[name]).get("github")
		if repo, _ := v.(string); repo != "" {
			out = append(out, Header{Project: name, GitHub: repo})
		}
	}
	return out, nil
}
