package config

import "runtime/debug"

// Build provenance, injected at link time.
//
// This lives in config rather than a package of its own because it IS
// configuration — resolved at startup from the environment the binary was
// built in, exactly as the rest of this package is resolved from the
// environment it runs in. Keeping it separate meant every caller imported two
// packages to answer one question.
//
// Values are set with -ldflags -X by the Taskfile. They are deliberately plain
// vars rather than constants so the linker can overwrite them, and they fall
// back to what Go's own VCS stamping records in debug.ReadBuildInfo when the
// binary was built without the Taskfile (for example `go run ./...`).

// Version is the release version, injected at link time. "dev" when unset.
var Version = "dev"

// Commit is the git commit the binary was built from, injected at link time.
// It is resolved from the embedded build info when the linker did not set it.
var Commit = ""

func init() {
	if Commit != "" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return
	}
	Commit = revision
	if modified == "true" {
		Commit += "-dirty"
	}
}
