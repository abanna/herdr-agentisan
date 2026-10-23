package daemon

import "context"

// Peer is the process at the other end of a connection to the daemon's
// socket, as the kernel told it when it connected (A60): never what the
// request says. Known is false when the kernel told nothing, as on an OS
// with no reader, and such a caller is placed nowhere.
type Peer struct {
	PID   int
	UID   uint32
	Known bool
}

type peerKey struct{}

// withPeer is ctx carrying p, the op's caller.
func withPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// peerFrom is the caller ctx carries; an unknown Peer when none.
func peerFrom(ctx context.Context) Peer {
	p, _ := ctx.Value(peerKey{}).(Peer)
	return p
}
