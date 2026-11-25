package bundle

import "os/exec"

var (
	ResolveWithinString            = resolveWithin[string]
	ReadInstalledStateWithResolver = readInstalledState
	VolumeWorktree                 = volumeWorktree
	GitTopLevel                    = gitTopLevel
)

type Worktree = worktree

func (w worktree) Paths() (repo, dir string) { return w.repo, w.dir }

func (l Launch) WithStart(start func(*exec.Cmd) error) Launch {
	l.start = start
	return l
}
