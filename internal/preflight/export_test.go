package preflight

import (
	"fmt"
	"io"
)

// KillGroup is what a run's Cancel does, with the kill a test gives it.
var KillGroup = killGroup

// MaxOutput is how much of tdaddy's output a run keeps.
const MaxOutput = maxOutput

// NewCapture is the writer a run gives tdaddy's stdout and stderr, with the
// run's own cap.
func NewCapture() interface {
	io.Writer
	fmt.Stringer
} {
	return &capped{max: maxOutput}
}
