package herdrtest

// HoldStreamsOpen makes s's Close run hold once its listener is closed and
// before it ends the open streams, standing in for a socket file that is
// slow to unlink.
func HoldStreamsOpen(s *Server, hold func()) { s.streamsEnd = hold }
