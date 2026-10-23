//go:build darwin

package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerOf is conn's peer from LOCAL_PEERPID and LOCAL_PEERCRED: the pid and
// uid of the process that connected. Anything but a Unix connection, or a
// read that fails, is an unknown Peer.
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
		pid, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID) //nolint:gosec // G115: a descriptor always fits an int
		if err != nil {
			return
		}
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED) //nolint:gosec // G115: a descriptor always fits an int
		if err == nil {
			p = Peer{PID: pid, UID: cred.Uid, Known: true}
		}
	})
	return p
}
