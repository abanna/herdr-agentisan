//go:build !linux && !darwin

package daemon

import "net"

// peerOf has no reader of a connection's peer here, so every caller is
// unknown, and a board op refuses it (A60): the check fails closed.
func peerOf(net.Conn) Peer { return Peer{} }
