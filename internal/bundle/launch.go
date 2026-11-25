package bundle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

const serverFile = ".claude/scripts/agentisan/mcp/server.py"

// Launch describes the server process and the client version for the handshake.
type Launch struct {
	Argv          []string
	Dir           string
	Env           []string // nil inherits the caller's environment
	ClientVersion string
	start         func(*exec.Cmd) error
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
		Argv: []string{
			uv, "--directory", repo, "--no-config", "run", "--no-project", "--isolated", "--no-env-file",
			"--with-editable", filepath.Join(repo, ".claude", "scripts"),
			"--", "python", "-I", "-m", "agentisan.mcp.server",
		},
		Dir:           repo,
		Env:           slices.DeleteFunc(os.Environ(), dropLaunchEnv),
		ClientVersion: clientVersion,
	}, nil
}

func dropLaunchEnv(entry string) bool {
	name, _, _ := strings.Cut(entry, "=")
	switch name {
	case "UV_OFFLINE", "UV_NO_PROGRESS":
		return false
	case "UV", "VIRTUAL_ENV", "CONDA_PREFIX", "_CONDA_ROOT", "__PYVENV_LAUNCHER__",
		"XDG_BIN_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_DIRS", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		"BASH_ENV", "ENV", "AGENTISAN_AGENT_SPAWN", "AGENTISAN_EXTERNAL_WRITES", "AGENTISAN_RUN_ID":
		return true
	}
	return strings.HasPrefix(name, "UV_") || strings.HasPrefix(name, "PYTHON") ||
		strings.HasPrefix(name, "_PYTHON") || strings.HasPrefix(name, "PYENV_") ||
		strings.HasPrefix(name, "PIP_") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_")
}
