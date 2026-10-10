//go:build !unix

package report

import (
	"fmt"
	"os"
)

// openNonBlocking opens path for reading. Outside unix there are no FIFOs to
// block on, so a plain open does.
func openNonBlocking(path string) (*os.File, error) {
	// #nosec G304 -- path is the transcript Codex names for the session this hook reports on; only a context percentage ever leaves the process.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}
