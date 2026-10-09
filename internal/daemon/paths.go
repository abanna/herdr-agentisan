// Package daemon is the agentisan daemon's lifecycle (ADR-001 D3, A3, A4): one
// daemon per herdr server, enforced by a flock'd lock that records the server
// it belongs to; a socket in the plugin state directory that answers JSON
// requests (D6); a protocol pin; and the start, stop and health operations the
// CLI and herdr's startup hook drive.
//
// The daemon itself runs an empty loop for now: it serves health and exits
// when its herdr server goes away. Collection and the store come later.
package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// File names inside the plugin state directory.
const (
	lockName   = "daemon.lock"
	logName    = "daemon.log"
	socketName = "daemon.sock"
)

// MaxSocketPath is the longest socket path the daemon binds: under 104 bytes,
// the macOS sun_path limit. Linux allows a little more, and the plugin ships
// for both (ADR-001 A3).
const MaxSocketPath = 103

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrNoStateDir means the plugin state directory is unset or relative.
	// herdr sets HERDR_PLUGIN_STATE_DIR, an absolute path, for every plugin
	// command it runs.
	ErrNoStateDir = errors.New("plugin state directory is not set to an absolute path")
	// ErrSocketPathTooLong means the daemon socket path would exceed
	// MaxSocketPath bytes.
	ErrSocketPathTooLong = errors.New("daemon socket path is too long")
	// ErrHerdrGone means the herdr server's socket is missing or is not a
	// socket: there is no server for a daemon to belong to.
	ErrHerdrGone = errors.New("herdr server socket is gone")
)

// Paths are the daemon's files in the plugin state directory.
type Paths struct {
	Dir    string
	Lock   string
	Log    string
	Socket string
}

// PathsFor returns the daemon's paths under stateDir.
func PathsFor(stateDir string) (Paths, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return Paths{}, fmt.Errorf("%w: %q", ErrNoStateDir, stateDir)
	}
	p := Paths{
		Dir:    stateDir,
		Lock:   filepath.Join(stateDir, lockName),
		Log:    filepath.Join(stateDir, logName),
		Socket: filepath.Join(stateDir, socketName),
	}
	if len(p.Socket) > MaxSocketPath {
		return Paths{}, fmt.Errorf("%w: %d bytes, at most %d: %s", ErrSocketPathTooLong, len(p.Socket), MaxSocketPath, p.Socket)
	}
	return p, nil
}

// Identity names one herdr server by its socket file. herdr binds a new socket
// on every start, live handoff included, so the inode changes whenever the
// server does.
type Identity struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// HerdrIdentity returns the identity of the herdr socket at path.
func HerdrIdentity(path string) (Identity, error) {
	if path == "" {
		return Identity{}, fmt.Errorf("%w: HERDR_SOCKET_PATH is not set", ErrHerdrGone)
	}
	st, err := os.Stat(path)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrHerdrGone, err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if st.Mode()&os.ModeSocket == 0 || !ok {
		return Identity{}, fmt.Errorf("%w: %s is not a socket", ErrHerdrGone, path)
	}
	return Identity{Dev: uint64(sys.Dev), Ino: sys.Ino}, nil //nolint:unconvert // Dev is int32 on darwin
}
