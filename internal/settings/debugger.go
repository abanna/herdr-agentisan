package settings

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// debuggerEnvFilePath is the env file the debugger's `events` commands read
// Elasticsearch's URL and read key from, each time they query (ADR-001 item 12).
const debuggerEnvFilePath = "debugger.env_file"

// debuggerSchema is [debugger]. Like [telemetry] it belongs to the shared
// file and --set only: the env file is this machine's, never a project's or
// a repo's.
var debuggerSchema = &node{kind: kindTable, fields: map[string]*node{
	"env_file": leafOf(kindString),
}}

// envFileHint is what an invalid env_file's error says.
const envFileHint = "not an absolute path or one under ~/"

// checkDebugger checks debugger.env_file is an absolute path, or one under
// ~/, which is expanded with HOME when it is read. Empty is allowed and
// means not configured.
func (m merged) checkDebugger() error {
	v, ok := m.get(debuggerEnvFilePath)
	if !ok {
		return nil
	}
	s, _ := v.(string)
	if s == "" || validEnvFile(s) {
		return nil
	}
	return m.invalidf(debuggerEnvFilePath, envFileHint)
}

func validEnvFile(s string) bool {
	if !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return false
	}
	if rest, ok := strings.CutPrefix(s, "~/"); ok {
		return rest != ""
	}
	return filepath.IsAbs(s)
}

// DebuggerEnvFile is debugger.env_file from the shared file in configDir,
// with a leading ~/ expanded through lookupEnv's HOME. "" is none, as are no
// configDir and no file in it. The read selects no project, and reads no
// environment variable but HOME.
func DebuggerEnvFile(configDir string, lookupEnv func(string) (string, bool)) (string, error) {
	if configDir == "" {
		return "", nil
	}
	res, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		return "", err
	}
	v, _ := lookup(res.Values, debuggerEnvFilePath)
	s, _ := v.(string)
	if s == "" {
		return "", nil
	}
	return ExpandHome(s, lookupEnv)
}
