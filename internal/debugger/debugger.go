// Package debugger is the debugger agent's launch (ADR-001 item 12, A32):
// the role that ships with the plugin, and the claude arguments and
// environment that start it read-only. `worker spawn --group debugger`
// launches it from Command (item 4, part 2); `debugger args` prints it for a
// launch by hand.
package debugger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

// sessionTools are Claude Code's own tools that act past the reads the
// launch allows: they start agents and workflows, schedule prompts, reach
// other sessions, send to claude.ai or the user's devices, switch worktrees
// or read an MCP server's resources. Most of them need no permission (the
// tools reference), so dontAsk, which denies only a call that would ask,
// never stops them. A deny rule naming a tool removes it from the session.
// EnterPlanMode is one: plan mode sends a command through the flow that
// asks the user, instead of denying it.
var sessionTools = []string{
	"Agent", "Workflow", "Monitor", "TaskStop", "EnterPlanMode",
	"CronCreate", "CronDelete", "CronList", "ScheduleWakeup", "RemoteTrigger",
	"SendMessage", "ListAgents", "PushNotification", "SendUserFile", "SendFeedback",
	"Artifact", "ArtifactData", "ArtifactComments", "DesignSync",
	"EnterWorktree", "ExitWorktree",
	"ListMcpResourcesTool", "ReadMcpResourceTool",
}

// mcpFile is a project's MCP config. Claude Code reads one in the directory
// a session starts in and in each directory above it, the root excepted.
const mcpFile = ".mcp.json"

// launchSettings is the launch's --settings. The fence: Claude Code's file
// tools refuse reads outside the working directories in every permission
// mode, and its file-reading Bash commands then ask, which dontAsk denies
// (Claude Code 2.1.257 or later). Without it, its built-in read-only
// commands, such as cat and grep, read any file the user can, with no
// allow rule. And the project's MCP servers, rejected by name.
type launchSettings struct {
	Permissions struct {
		BlockReadsOutsideWorkingDirectories bool `json:"blockReadsOutsideWorkingDirectories"`
	} `json:"permissions"`
	DisabledMcpjsonServers []string `json:"disabledMcpjsonServers,omitempty"`
}

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

// Command is the debugger's agent invocation: the directory it starts in,
// the arguments after claude, and the environment its process needs. Dir is
// part of the fence: claude has no flag for its working directory, and the
// one it starts in is a working directory, readable like the repo.
type Command struct {
	Dir  string            `json:"dir"`
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

// Command builds the launch. The role is appended from the plugin root as
// written; a second, short prompt gives the paths only the launch knows.
// In dontAsk mode a call that would ask is denied without asking: the
// debugger reads the repo (not its .env files) and the daemon's directory,
// runs `<Bin> events` and Claude Code's built-in read-only commands, and
// reads Linear. The tools that never ask and act past that (sessionTools)
// are denied by name. Its reads are fenced to its working directories, the
// repo it starts in (Dir) and the daemon's directory (--add-dir), by
// launchSettings, which also rejects every server the project's MCP
// configs name (projectServers), so none loads or asks for approval at
// start. claude.ai's connectors, Linear's among them, still load:
// --strict-mcp-config would keep them out too. Only the project's settings load
// (D12's bundle), so the allow rules that pile up in the user's and the
// repo's local settings never widen it; the repo's .claude/settings.json,
// tracked by git or not, and managed policy still apply. Each key file is
// denied to Read, and neither the repo nor the daemon's directory may hold
// one. It carries no --model or --name: the workforce sets those.
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
	deny = append(deny, sessionTools...)

	var set launchSettings
	set.Permissions.BlockReadsOutsideWorkingDirectories = true
	servers, err := projectServers(l.Repo)
	if err != nil {
		return Command{}, err
	}
	set.DisabledMcpjsonServers = servers
	settingsJSON, err := json.Marshal(set)
	if err != nil {
		return Command{}, fmt.Errorf("%w: the settings: %w", ErrInvalid, err)
	}

	args := make([]string, 0, 14+len(allow)+len(deny))
	args = append(args,
		"--append-system-prompt-file", filepath.Join(l.Root, RoleFile),
		"--append-system-prompt", l.facts(),
		"--setting-sources", "project",
		"--permission-mode", "dontAsk",
		"--settings", string(settingsJSON))
	if l.ServerDir != "" {
		args = append(args, "--add-dir", l.ServerDir)
	}
	args = append(args, "--allowedTools")
	args = append(args, allow...)
	args = append(args, "--disallowedTools")
	args = append(args, deny...)
	return Command{Dir: l.Repo, Args: args, Env: map[string]string{trail.ConfigDirEnv: l.ConfigDir}}, nil
}

// projectServers is every server named in the MCP configs a session started
// in dir reads: the .mcp.json in dir and in each directory above it, the
// root excepted. Claude Code asks before it loads a server from one that it
// has not been told about; a server named in disabledMcpjsonServers is
// rejected instead, with no question. A config that is not a regular file,
// or cannot be read or parsed, refuses the launch, since what it would load
// is unknown; a FIFO is never opened, so it cannot hang the launch. A server
// added after the launch is built would ask.
func projectServers(dir string) ([]string, error) {
	var names []string
	for ; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		path := filepath.Join(dir, mcpFile)
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil && !info.Mode().IsRegular() {
			err = errors.New("not a regular file")
		}
		var data []byte
		if err == nil {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: the MCP config %s cannot be read: %w", ErrInvalid, path, err)
		}
		var config map[string]json.RawMessage
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("%w: the MCP config %s is not a JSON object: %w", ErrInvalid, path, err)
		}
		if raw, ok := config["mcpServers"]; ok {
			if err := json.Unmarshal(raw, &servers); err != nil {
				return nil, fmt.Errorf("%w: the MCP config %s: mcpServers is not an object: %w", ErrInvalid, path, err)
			}
		}
		for name := range servers {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
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
