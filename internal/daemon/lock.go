package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockVersion is the schema version of LockInfo.
const lockVersion = 1

// LockInfo is what the daemon holding daemon.lock writes into it: who holds
// it, since when, and which herdr server it belongs to.
type LockInfo struct {
	V   int `json:"v"`
	PID int `json:"pid"`
	// Start is the holder's process start time (clock ticks since boot), or 0
	// where it cannot be read. It tells the holder apart from a later process
	// that reuses its pid.
	Start     uint64    `json:"start"`
	Herdr     Identity  `json:"herdr"`
	StartedAt time.Time `json:"started_at"`
}

// valid reports whether the record names a holder.
func (i LockInfo) valid() bool { return i.V == lockVersion && i.PID > 1 }

// errLocked means another open file holds the lock.
var errLocked = errors.New("lock is held")

// openLock opens the lock file without truncating it: a contender must never
// wipe the holder's record. Only the holder truncates, after it has the lock.
func openLock(path string) (*os.File, error) {
	// #nosec G304 -- path is the daemon's own lock under the plugin state dir herdr names.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}

// tryLock takes an exclusive flock on f without blocking. The lock belongs to
// the open file and is released when f is closed.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLocked
	}
	if err != nil {
		return fmt.Errorf("flock %s: %w", f.Name(), err)
	}
	return nil
}

// writeInfo replaces the lock file's record. Only the holder calls it.
func writeInfo(f *os.File, info LockInfo) error {
	raw, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("encode lock record: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate %s: %w", f.Name(), err)
	}
	if _, err := f.WriteAt(append(raw, '\n'), 0); err != nil {
		return fmt.Errorf("write %s: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", f.Name(), err)
	}
	return nil
}

// readInfo reads the lock record. A record that is empty, partial or of
// another version reads as the zero LockInfo, which is not valid.
func readInfo(path string) LockInfo {
	// #nosec G304 -- path is the daemon's own lock under the plugin state dir herdr names.
	raw, err := os.ReadFile(path)
	if err != nil {
		return LockInfo{}
	}
	var info LockInfo
	if json.Unmarshal(raw, &info) != nil || !info.valid() {
		return LockInfo{}
	}
	return info
}

// held reports whether a live process holds the lock at path. It takes the
// lock for an instant when it is free.
func held(path string) (bool, error) {
	f, err := openLock(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // no daemon has ever run for this server
	}
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck // closing releases the probe's lock; nothing was written
	switch err := tryLock(f); {
	case errors.Is(err, errLocked):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}
