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

// ErrInvalid means a launch input is not usable.
var ErrInvalid = errors.New("invalid debugger launch")

// LinearReadTools are the Linear tools the debugger may call: reads only.
var LinearReadTools = []string{
	"mcp__claude_ai_Linear__get_issue",
	"mcp__claude_ai_Linear__list_issues",
	"mcp__claude_ai_Linear__list_comments",
}

// deniedTools are refused outright, whatever the permission mode: the
// debugger changes nothing and reaches nothing outside its allow list.
var deniedTools = []string{"Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch"}

// Launch is what a debugger's launch is built from: the plugin's checkout
// (Root) and binary (Bin), the project's repo it runs in, the daemon's
// directory for this herdr server (ServerDir, holding daemon.log; "" when
// unknown), and the plugin's config dir. Every path is absolute.
type Launch struct {
	Root      string
	Bin       string
	Repo      string
	ServerDir string
	ConfigDir string
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
// runs `<Bin> events`, and reads Linear, and nothing else. It carries no
// --model or --name: the workforce sets those.
func (l Launch) Command() (Command, error) {
	for _, p := range []struct{ name, path string }{
		{"root", l.Root}, {"binary", l.Bin}, {"repo", l.Repo}, {"config dir", l.ConfigDir}, {"server dir", l.ServerDir},
	} {
		if p.path == "" && p.name == "server dir" {
			continue
		}
		if err := checkPath(p.path); err != nil {
			return Command{}, fmt.Errorf("%w: the %s %w", ErrInvalid, p.name, err)
		}
	}
	allow := []string{readRule(l.Repo)}
	if l.ServerDir != "" {
		allow = append(allow, readRule(l.ServerDir))
	}
	allow = append(allow, "Bash("+l.Bin+" events:*)")
	allow = append(allow, LinearReadTools...)
	deny := append([]string{"Read(/" + l.Repo + "/**/.env*)"}, deniedTools...)

	args := make([]string, 0, 8+len(allow)+len(deny))
	args = append(args,
		"--append-system-prompt-file", filepath.Join(l.Root, RoleFile),
		"--append-system-prompt", l.facts(),
		"--permission-mode", "dontAsk",
		"--allowedTools")
	args = append(args, allow...)
	args = append(args, "--disallowedTools")
	args = append(args, deny...)
	return Command{Args: args, Env: map[string]string{trail.ConfigDirEnv: l.ConfigDir}}, nil
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
