//go:build !unix

package settings

import (
	"fmt"
	"os"
)

// openNonBlocking opens path for reading. Outside unix there are no FIFOs to
// block on, so a plain open does.
func openNonBlocking(path string) (*os.File, error) {
	// #nosec G304 -- path is the plugin's own config file or a configured repo's overlay, both chosen by the user who runs this.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}
