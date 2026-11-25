package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

const (
	osc11Prefix = "\x1b]11;"
	xRGBPrefix  = "rgb:"
	maxXRGB     = len(xRGBPrefix) + 4*3 + 2 // three channels and two slashes
	decoderIdle = uv.DefaultEscTimeout + time.Millisecond
)

// terminalInput retains Bubble Tea's raw-mode interface. The read pump
// owns cancellation on the original input descriptor.
type terminalInput interface {
	io.ReadWriteCloser
	Fd() uintptr
}

type osc11File struct {
	terminalInput
	reader *osc11Reader
}

func (f *osc11File) Read(p []byte) (int, error) { return f.reader.Read(p) }

func normalizeTerminalInput(ctx context.Context, in io.Reader) (io.Reader, func()) {
	if in == nil {
		return nil, func() {}
	}
	source := newTerminalReads(ctx, in)
	reader := &osc11Reader{next: source.next}
	if terminal, ok := in.(terminalInput); ok {
		return &osc11File{terminalInput: terminal, reader: reader}, source.stop
	}
	return reader, source.stop
}

// osc11Reader holds only candidate colour bytes. An empty read tells the
// decoder about progress while those bytes are still being collected.
type osc11Reader struct {
	next      func(time.Time, bool) terminalChunk
	matched   int
	body      []byte
	passOSC   bool
	expires   time.Time
	keepalive time.Time
	resume    time.Time
	lastRead  time.Time
	lag       time.Duration
	deferred  *terminalChunk
	pending   []byte
	readErr   error
}

func (r *osc11Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) != 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.readErr != nil {
		err := r.readErr
		r.readErr = nil
		return 0, err
	}

	out, err := r.readChunk(p[:0])
	n := copy(p, out)
	if n < len(out) {
		r.pending = out[n:]
	}
	if n > 0 {
		// The terminal decoder discards bytes returned alongside an error.
		r.readErr = err
		return n, nil
	}
	return 0, err
}

func (r *osc11Reader) readChunk(out []byte) ([]byte, error) {
	deadline := r.keepalive
	if deadline.IsZero() && len(r.body) != 0 {
		deadline = r.expires
	}
	chunk := r.nextChunk(deadline)
	if errors.Is(chunk.err, errTerminalReadTimeout) {
		if !r.keepalive.IsZero() {
			r.keepalive = time.Time{}
			return out, nil
		}
		return r.expire(out), nil
	}
	if !r.expires.IsZero() && !chunk.at.Before(r.expires) {
		r.resetReply()
	}
	for _, b := range chunk.data {
		out = r.consume(out, b)
	}
	if r.matched > 0 {
		r.expires = time.Now().Add(uv.DefaultEscTimeout)
	}
	if len(r.body) != 0 {
		r.keepalive = r.expires.Add(-uv.DefaultEscTimeout / 2)
	}
	if chunk.err != nil {
		out = r.release(out)
	}
	return out, chunk.err
}

func (r *osc11Reader) nextChunk(deadline time.Time) terminalChunk {
	var chunk terminalChunk
	if r.deferred == nil {
		chunk = r.next(deadline, r.lag > 0 || !r.resume.IsZero())
		if chunk.at.IsZero() {
			return chunk
		}
		// After releasing an expired reply, preserve the spacing of input
		// that arrived during the decoder's idle interval. Catch up only
		// across enough idle time that the decoder still sees a timeout.
		slack := chunk.at.Sub(r.lastRead) - decoderIdle
		r.lag = max(0, r.lag-max(0, slack), r.resume.Sub(chunk.at))
		r.resume = time.Time{}
		r.lastRead = chunk.at
		chunk.at = chunk.at.Add(r.lag)
	} else {
		chunk = *r.deferred
		r.deferred = nil
		if r.resume.After(chunk.at) {
			r.lag += r.resume.Sub(chunk.at)
			chunk.at = r.resume
		}
		r.resume = time.Time{}
	}
	if !deadline.IsZero() && !chunk.at.Before(deadline) {
		r.deferred = &chunk
		time.Sleep(time.Until(deadline))
		return terminalChunk{err: errTerminalReadTimeout}
	}
	time.Sleep(time.Until(chunk.at))
	return chunk
}

// Halfway through an idle interval, an empty read keeps the introducer in
// the decoder until we can release the original payload. Give the decoder
// an idle interval to expire those bytes before forwarding the next read.
// This preserves the unknown event without turning the payload into keys.
func (r *osc11Reader) expire(out []byte) []byte {
	r.resume = time.Now().Add(decoderIdle)
	return r.release(out)
}

func (r *osc11Reader) release(out []byte) []byte {
	out = append(out, r.body...)
	r.resetReply()
	return out
}

func (r *osc11Reader) resetReply() {
	r.body = r.body[:0]
	r.matched = 0
	r.passOSC = false
	r.expires = time.Time{}
	r.keepalive = time.Time{}
}

func (r *osc11Reader) consume(out []byte, b byte) []byte {
	if r.matched == len(osc11Prefix) {
		switch {
		case b == ansi.BEL || b == ansi.ST || b == ansi.ESC:
			// The decoder decides whether Escape cancels the OSC or
			// terminates it with the next backslash. In either case it
			// needs the Escape immediately, with any complete colour.
			return r.finish(out, b)
		case b == ansi.CAN || b == ansi.SUB:
			return append(r.release(out), b)
		case !r.passOSC && xRGBByte(len(r.body), b):
			r.body = append(r.body, b)
			return out
		}
		out = append(out, r.body...)
		r.body = r.body[:0]
		r.keepalive = time.Time{}
		r.passOSC = true
		return append(out, b)
	}
	switch b {
	case osc11Prefix[r.matched]:
		r.matched++
	case ansi.ESC:
		r.matched = 1
	default:
		r.resetReply()
	}
	return append(out, b)
}

func xRGBByte(offset int, b byte) bool {
	if offset < len(xRGBPrefix) {
		return b|0x20 == xRGBPrefix[offset]
	}
	return offset < maxXRGB && strings.ContainsRune("0123456789abcdefABCDEF/", rune(b))
}

func (r *osc11Reader) finish(out []byte, terminator byte) []byte {
	if rgb, ok := parseXRGB(string(r.body)); ok {
		// The hex spelling is recognized by x/ansi and is shorter than an
		// rgb: reply even when all three input channels are one digit.
		out = fmt.Appendf(out, "#%02x%02x%02x", rgb[0], rgb[1], rgb[2])
	} else {
		out = append(out, r.body...)
	}
	r.resetReply()
	return append(out, terminator)
}

func parseXRGB(payload string) ([3]uint64, bool) {
	var rgb [3]uint64
	body, ok := strings.CutPrefix(strings.ToLower(payload), xRGBPrefix)
	channels := strings.Split(body, "/")
	if !ok || len(channels) != len(rgb) {
		return rgb, false
	}
	for i, channel := range channels {
		width := len(channel)
		if width == 0 || width > 4 {
			return rgb, false
		}
		value, err := strconv.ParseUint(channel, 16, 16)
		if err != nil {
			return rgb, false
		}
		limit := uint64(1)<<(4*width) - 1
		rgb[i] = (value*255 + limit/2) / limit
	}
	return rgb, true
}
