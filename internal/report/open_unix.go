//go:build unix

package report

import (
	"fmt"
	"os"
	"syscall"
)

// openNonBlocking opens path for reading without blocking: a FIFO opened
// for reading otherwise waits for a writer forever, before its type can be
// checked.
func openNonBlocking(path string) (*os.File, error) {
	// #nosec G304 -- path is the transcript Codex names for the session this hook reports on; only a context percentage ever leaves the process.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}
