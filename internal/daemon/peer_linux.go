//go:build linux

package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerOf is conn's peer from SO_PEERCRED: the pid and uid of the process
// that connected. Anything but a Unix connection, or a read that fails,
// is an unknown Peer.
func peerOf(conn net.Conn) Peer {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Peer{}
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Peer{}
	}
	var p Peer
	_ = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err == nil {
			p = Peer{PID: int(cred.Pid), UID: cred.Uid, Known: true}
		}
	})
	return p
}
