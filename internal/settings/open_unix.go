//go:build unix

package settings

import (
	"fmt"
	"os"
	"syscall"
)

// openNonBlocking opens path for reading without blocking: a FIFO opened
// for reading otherwise waits for a writer forever, before its type can be
// checked.
func openNonBlocking(path string) (*os.File, error) {
	// #nosec G304 -- path is the plugin's own config file or a configured repo's overlay, both chosen by the user who runs this.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}
