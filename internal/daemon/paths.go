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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
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

// Paths are one herdr server's daemon files: a directory under the plugin
// state directory, keyed by the server's socket path. herdr gives every
// session its own socket path and shares the plugin state directory between
// them, so the key is what makes the daemon one per server (D3). A live
// handoff keeps the socket path, so the directory, and the lock in it, carry
// across it (A3).
type Paths struct {
	Dir    string
	Lock   string
	Log    string
	Socket string
}

// PathsFor returns the paths of the daemon for the herdr server at
// herdrSocket, under stateDir.
func PathsFor(stateDir, herdrSocket string) (Paths, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return Paths{}, fmt.Errorf("%w: %q", ErrNoStateDir, stateDir)
	}
	if herdrSocket == "" || !filepath.IsAbs(herdrSocket) {
		return Paths{}, fmt.Errorf("%w: HERDR_SOCKET_PATH %q is not an absolute path", ErrHerdrGone, herdrSocket)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(herdrSocket)))
	dir := filepath.Join(stateDir, "srv-"+hex.EncodeToString(sum[:6]))
	p := Paths{
		Dir:    dir,
		Lock:   filepath.Join(dir, lockName),
		Log:    filepath.Join(dir, logName),
		Socket: filepath.Join(dir, socketName),
	}
	if len(p.Socket) > MaxSocketPath {
		return Paths{}, fmt.Errorf("%w: %d bytes, at most %d: %s", ErrSocketPathTooLong, len(p.Socket), MaxSocketPath, p.Socket)
	}
	return p, nil
}

// Identity names one herdr server by the socket it bound. herdr binds a new
// socket on every start, live handoff included. The inode number alone is
// not enough: a filesystem may hand the new socket the old one's number, so
// the socket's birth time (its change time where the filesystem keeps no
// birth time) tells them apart.
type Identity struct {
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
	Born int64  `json:"born"`
}

// HerdrIdentity returns the identity of the herdr socket at path.
func HerdrIdentity(path string) (Identity, error) {
	if path == "" {
		return Identity{}, fmt.Errorf("%w: HERDR_SOCKET_PATH is not set", ErrHerdrGone)
	}
	id, isSocket, err := socketIdentity(path)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrHerdrGone, err)
	}
	if !isSocket {
		return Identity{}, fmt.Errorf("%w: %s is not a socket", ErrHerdrGone, path)
	}
	return id, nil
}
