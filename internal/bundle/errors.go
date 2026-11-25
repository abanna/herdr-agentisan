// Package bundle is the client of the installed Agentisan bundle's MCP server
// (NERD-5790): the plugin reaches the bundle only through that server, never
// the other way. A Session starts it over stdio in a process group of its own,
// speaks the pinned handshake and runs read operations through its `execute`
// tool (dry_run is always true). The context given to Open bounds the whole
// session, and when it ends the group is killed.
package bundle

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Sentinel errors. Callers branch on them with errors.Is, never on a
// message; the text a sentinel carries is bounded data for a person.
var (
	// ErrUnavailable means the bundle cannot be used: not installed, no uv,
	// the server would not start or died before its handshake, an
	// incompatible version, or an operation it does not have.
	ErrUnavailable = errors.New("the agentisan bundle is unavailable")
	// ErrVersion means the server's handshake is not the pinned one:
	// another protocol, name or bundle version below 0.19.0, or no tools.
	ErrVersion = fmt.Errorf("the agentisan bundle's version is not supported: %w", ErrUnavailable)
	// ErrUnknownOperation means the bundle has no such operation.
	ErrUnknownOperation = fmt.Errorf("the agentisan bundle has no such operation: %w", ErrUnavailable)
	// ErrRefused means the call was refused: the arguments, the bundle's
	// policy, or a work dir outside the repo.
	ErrRefused = errors.New("the call was refused")
	// ErrOperation means the operation ran and failed.
	ErrOperation = errors.New("the agentisan operation failed")
	// ErrTimeout means the context ended before the server answered: the
	// group was killed.
	ErrTimeout = errors.New("the agentisan bundle did not answer in time")
	// ErrServerDied means the server ended, or its stdout closed, before it
	// answered.
	ErrServerDied = errors.New("the agentisan bundle's server died")
	// ErrProtocol means the server wrote something that is not the pinned
	// MCP: a line that is not JSON, a reply to another request, a request of
	// its own, or no payload.
	ErrProtocol = errors.New("the agentisan bundle's server broke the protocol")
	// ErrContract means a reply does not have the shape this client pins.
	ErrContract = errors.New("the agentisan bundle's reply does not match the pinned contract")
)

// maxText bounds what an error carries of the server's own words.
const maxText = 200

// clip makes a server's words safe to put in an error: valid UTF-8, no
// control characters, at most maxText bytes.
func clip(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if len(s) > maxText {
		s = s[:maxText]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "..."
	}
	return s
}
