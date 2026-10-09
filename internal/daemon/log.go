package daemon

import (
	"fmt"
	"os"
	"sync"
)

// LogCap is daemon.log's size cap: 10 MB, with one rotated file beside it.
const LogCap = 10 << 20

// LogWriter appends to daemon.log and keeps it under its cap. The write that
// would cross the cap first moves the file to daemon.log.1, replacing any
// older one, so at most two files exist. Logs live in a plain file rather
// than the state database, so the log survives whatever broke the database.
type LogWriter struct {
	mu   sync.Mutex
	path string
	cap  int64
	f    *os.File
	size int64
}

// NewLogWriter opens path for appending, owner-only.
func NewLogWriter(path string, capBytes int64) (*LogWriter, error) {
	w := &LogWriter{path: path, cap: capBytes}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *LogWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("%w: open %s: %w", ErrLog, w.path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("%w: stat %s: %w", ErrLog, w.path, err)
	}
	w.f, w.size = f, st.Size()
	return nil
}

// Write appends p, rotating first when p would take the file past its cap. A
// single write larger than the cap still lands whole, in a fresh file.
func (w *LogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size > 0 && w.size+int64(len(p)) > w.cap {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("%w: write %s: %w", ErrLog, w.path, err)
	}
	return n, nil
}

func (w *LogWriter) rotate() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %w", ErrLog, w.path, err)
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return fmt.Errorf("%w: rotate %s: %w", ErrLog, w.path, err)
	}
	return w.open()
}

// Close closes the log file.
func (w *LogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %w", ErrLog, w.path, err)
	}
	return nil
}
