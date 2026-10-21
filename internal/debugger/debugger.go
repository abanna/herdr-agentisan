// Package debugger is the debugger agent's launch (ADR-001 item 12, A32):
// the role that ships with the plugin, and the claude arguments and
// environment that start it read-only. `worker spawn --group debugger`
// launches it from Command (item 4, part 2); `debugger args` prints it for a
// launch by hand.
package debugger

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abanna/herdr-agentisan/internal/trail"
)

// RoleFile is the role's path under the plugin root.
const RoleFile = "roles/debugger.md"

// serverDirName names Launch.ServerDir in errors; it alone may be empty.
const serverDirName = "server dir"

// ErrInvalid means a launch input is not usable.
var ErrInvalid = errors.New("invalid debugger launch")

// LinearReadTools are the Linear tools the debugger may call: reads only.
var LinearReadTools = []string{
	"mcp__claude_ai_Linear__get_issue",
	"mcp__claude_ai_Linear__list_issues",
	"mcp__claude_ai_Linear__list_comments",
}

// deniedTools are refused outright, whatever the permission mode or a
// setting allows: a deny rule wins over every allow rule.
var deniedTools = []string{"Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch"}

// Launch is what a debugger's launch is built from: the plugin's checkout
// (Root) and binary (Bin), the project's repo it runs in, the daemon's
// directory for this herdr server (ServerDir, holding daemon.log; "" when
// unknown), the plugin's config dir, and KeyFiles, the files that hold the
// trail's read key: the configured env file, and its target when it is a
// link. Every path is absolute.
type Launch struct {
	Root      string
	Bin       string
	Repo      string
	ServerDir string
	ConfigDir string
	KeyFiles  []string
}

// Command is the debugger's agent invocation: the arguments after claude,
// and the environment its process needs.
type Command struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

// Command builds the launch. The role is appended from the plugin root as
// written; a second, short prompt gives the paths only the launch knows.
// In dontAsk mode a tool with no allow rule is denied without asking: the
// debugger reads the repo (not its .env files) and the daemon's directory,
// runs `<Bin> events`, and reads Linear. Only the project's settings load
// (D12's bundle), so the allow rules that pile up in the user's and the
// repo's local settings never widen it; the repo's .claude/settings.json,
// tracked or installed (the Agentisan bundle), and managed policy still
// apply. Each key file is denied to Read, and neither
// the repo nor the daemon's directory may hold one. It carries no --model
// or --name: the workforce sets those.
func (l Launch) Command() (Command, error) {
	if len(l.KeyFiles) == 0 {
		return Command{}, fmt.Errorf("%w: no env file holds the trail's key", ErrInvalid)
	}
	paths := []struct{ name, path string }{
		{"root", l.Root}, {"binary", l.Bin}, {"repo", l.Repo}, {"config dir", l.ConfigDir}, {serverDirName, l.ServerDir},
	}
	for _, k := range l.KeyFiles {
		paths = append(paths, struct{ name, path string }{"env file", k})
	}
	for _, p := range paths {
		if p.path == "" && p.name == serverDirName {
			continue
		}
		if err := checkPath(p.path); err != nil {
			return Command{}, fmt.Errorf("%w: the %s %w", ErrInvalid, p.name, err)
		}
	}
	for _, k := range l.KeyFiles {
		for _, dir := range []struct{ name, path string }{{"repo", l.Repo}, {serverDirName, l.ServerDir}} {
			if dir.path != "" && holds(dir.path, k) {
				return Command{}, fmt.Errorf("%w: the %s %s holds the trail's env file %s", ErrInvalid, dir.name, dir.path, k)
			}
		}
	}
	allow := []string{readRule(l.Repo)}
	if l.ServerDir != "" {
		allow = append(allow, readRule(l.ServerDir))
	}
	allow = append(allow, "Bash("+l.Bin+" events:*)")
	allow = append(allow, LinearReadTools...)
	deny := []string{"Read(/" + l.Repo + "/**/.env*)"}
	for _, k := range l.KeyFiles {
		deny = append(deny, "Read(/"+k+")")
	}
	deny = append(deny, deniedTools...)

	args := make([]string, 0, 10+len(allow)+len(deny))
	args = append(args,
		"--append-system-prompt-file", filepath.Join(l.Root, RoleFile),
		"--append-system-prompt", l.facts(),
		"--setting-sources", "project",
		"--permission-mode", "dontAsk",
		"--allowedTools")
	args = append(args, allow...)
	args = append(args, "--disallowedTools")
	args = append(args, deny...)
	return Command{Args: args, Env: map[string]string{trail.ConfigDirEnv: l.ConfigDir}}, nil
}

// holds reports whether dir is path or one of its ancestors. Both are
// absolute and clean (checkPath).
func holds(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// readRule allows reading everything under dir: Claude's rules spell an
// absolute path with a leading //.
func readRule(dir string) string { return "Read(/" + dir + "/**)" }

// facts is the second prompt: the paths the role file refers to.
func (l Launch) facts() string {
	log := "daemon.log was not given to this session."
	if l.ServerDir != "" {
		log = "daemon.log is " + filepath.Join(l.ServerDir, "daemon.log") + " (rotated to daemon.log.1)."
	}
	return "Debugger session facts: the plugin binary is " + l.Bin + ", so run `" + l.Bin + " events worker <id>` and `" +
		l.Bin + " events query '<ES|QL>'`; the project repo is " + l.Repo + "; " + log
}

// checkPath refuses a path that is not absolute and clean, not UTF-8 (claude
// would read another path than the rule names, and JSON would print it
// changed), or that holds a character Claude's rule syntax or argument lists
// would read as structure.
func checkPath(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return errors.New("is not an absolute, clean path")
	}
	if !utf8.ValidString(p) {
		return errors.New("is not valid UTF-8")
	}
	if i := strings.IndexFunc(p, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`,()[]{}*?"'\`+"`", r)
	}); i >= 0 {
		r, _ := utf8.DecodeRuneInString(p[i:])
		return fmt.Errorf("holds %q, which a tool rule cannot carry", r)
	}
	return nil
}
