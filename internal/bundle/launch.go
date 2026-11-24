package bundle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// serverFile is the bundle's MCP server, under the repo.
const serverFile = ".claude/scripts/agentisan/mcp/server.py"

// Launch is how to start a server: the command, where, with what
// environment (nil: the caller's) and the client's own version, which the
// handshake tells the server.
type Launch struct {
	Argv          []string
	Dir           string
	Env           []string
	ClientVersion string
}

// Locate finds the bundle installed in repo, the project's main checkout:
// the version marker and the server file must be regular files and uv must
// be on the PATH. The command is the bundle's own template with
// --no-project, so uv never syncs a project environment or writes a uv.lock
// in the repo (safe for every read operation, not for verify or Job 1).
func Locate(repo, clientVersion string) (Launch, error) {
	if !filepath.IsAbs(repo) {
		return Launch{}, fmt.Errorf("%w: the repo %q is not an absolute path", ErrRefused, repo)
	}
	for _, rel := range []string{settings.BundleMarker, serverFile} {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		info, err := os.Stat(path)
		if err != nil {
			return Launch{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		if !info.Mode().IsRegular() {
			return Launch{}, fmt.Errorf("%w: %s is not a regular file", ErrUnavailable, path)
		}
	}
	uv, err := exec.LookPath("uv")
	if err == nil {
		uv, err = filepath.Abs(uv)
	}
	if err != nil {
		return Launch{}, fmt.Errorf("%w: uv: %w", ErrUnavailable, err)
	}
	return Launch{
		Argv:          []string{uv, "run", "--no-project", "--with-editable", ".claude/scripts", "python", "-m", "agentisan.mcp.server"},
		Dir:           repo,
		Env:           os.Environ(),
		ClientVersion: clientVersion,
	}, nil
}
