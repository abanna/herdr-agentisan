//go:build !unix

package settings

import "os/exec"

// killGroup leaves exec's default cancellation, which kills op alone:
// outside unix there is no process group to signal.
func killGroup(*exec.Cmd) {}
