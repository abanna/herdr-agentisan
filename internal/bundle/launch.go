package bundle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

const serverFile = ".claude/scripts/agentisan/mcp/server.py"

// Launch describes the server process and the client version for the handshake.
type Launch struct {
	Argv          []string
	Dir           string
	Env           []string // nil inherits the caller's environment
	ClientVersion string
}

// Locate finds the installed bundle and uv. For reads, --no-project skips
// project environment sync and uv.lock writes.
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
