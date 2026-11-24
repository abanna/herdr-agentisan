package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

const terminalReadSize = 4096

var errTerminalReadTimeout = errors.New("terminal reply timed out")

type terminalChunk struct {
	data []byte
	at   time.Time
	err  error
}

// terminalReads waits on an input read and the reply deadline separately.
// Bubble Tea still sees the terminal's descriptor for raw mode, but its
// reader calls through here so a quiet descriptor cannot stall expiry.
type terminalReads struct {
	ctx     context.Context
	cancel  context.CancelFunc
	input   io.Reader
	start   sync.Once
	request chan struct{}
	result  chan terminalChunk
	waiting bool

	cancelDone  chan struct{}
	done        chan struct{}
	interrupted bool
}

func newTerminalReads(ctx context.Context, input io.Reader) *terminalReads {
	ctx, cancel := context.WithCancel(ctx)
	return &terminalReads{
		ctx: ctx, cancel: cancel, input: input,
		request: make(chan struct{}), result: make(chan terminalChunk),
		cancelDone: make(chan struct{}), done: make(chan struct{}),
	}
}

func (r *terminalReads) stop() {
	r.cancel()
	r.start.Do(func() {
		close(r.cancelDone)
		close(r.done)
	})
	<-r.cancelDone
	// A descriptor-backed read must finish before the caller can close its
	// file. A plain io.Reader may have no way to interrupt a blocked Read.
	if r.interrupted {
		<-r.done
	}
}

func (r *terminalReads) next(deadline time.Time) terminalChunk {
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return terminalChunk{err: errTerminalReadTimeout}
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		timeout = timer.C
	}
	r.start.Do(func() { go r.readLoop() })
	var request chan<- struct{}
	if !r.waiting {
		request = r.request
	}

	for {
		select {
		case <-r.ctx.Done():
			return terminalChunk{err: io.EOF}
		case <-timeout:
			return terminalChunk{err: errTerminalReadTimeout}
		case request <- struct{}{}:
			r.waiting = true
			request = nil
		case chunk, ok := <-r.result:
			r.waiting = false
			if !ok {
				return terminalChunk{err: io.EOF}
			}
			return chunk
		}
	}
}

func (r *terminalReads) readLoop() {
	defer close(r.done)
	defer close(r.result)
	source, err := uv.NewCancelReader(r.input)
	if err != nil {
		close(r.cancelDone)
		r.send(terminalChunk{err: fmt.Errorf("open dashboard terminal input: %w", err)})
		return
	}
	stop := context.AfterFunc(r.ctx, func() {
		r.interrupted = source.Cancel()
		close(r.cancelDone)
	})
	defer func() {
		if stop() {
			close(r.cancelDone)
		} else {
			<-r.cancelDone
		}
		_ = source.Close()
	}()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.request:
		}
		var buf [terminalReadSize]byte
		n, err := source.Read(buf[:])
		r.send(terminalChunk{data: buf[:n], at: time.Now(), err: err})
	}
}

func (r *terminalReads) send(chunk terminalChunk) {
	select {
	case <-r.ctx.Done():
	case r.result <- chunk:
	}
}
