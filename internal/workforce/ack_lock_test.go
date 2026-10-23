package workforce_test

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// TestHandoffAckSharesRetireLock (QA #140 F9) holds Retire inside its locked
// close and starts ack against another pane. An ack-only mutex must not let
// ack read herdr, rename agents or complete while Retire owns the workforce
// lock. The observation window is bounded, not a proof of every schedule.
func TestHandoffAckSharesRetireLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	next, h := rotating(t, f, w)
	f.stated(w.PaneID, "NERD-5261", "BOSS: handoff written")
	f.stated(next, "NERD-5261", "BOSS: handoff written")
	spare := f.h.addPane(w.PaneID, "", "")
	before := f.row(t, w.ID)
	rec := &steps{}
	f.svc.HandoffSteps = rec.record

	inside, release := make(chan struct{}), make(chan struct{})
	violation := make(chan string, 1)
	var entered, finish sync.Once
	var released atomic.Bool
	unblock := func() {
		finish.Do(func() {
			released.Store(true)
			close(release)
		})
	}
	defer unblock()
	server := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
		var params struct {
			PaneID string `json:"pane_id"`
		}
		if len(r.Params) > 0 {
			if err := json.Unmarshal(r.Params, &params); err != nil {
				return errReply("bad_request")
			}
		}
		if r.Method == "pane.get" && params.PaneID == spare {
			// Retire reaches closeProven only after taking the shared lock.
			// Pause outside the fake's mutex, so an incorrect ack can proceed.
			entered.Do(func() { close(inside) })
			<-release
		}
		if !released.Load() {
			select {
			case <-inside:
				if r.Method == "pane.list" || r.Method == "agent.list" || r.Method == "agent.rename" ||
					(r.Method == "pane.get" && params.PaneID == next) {
					select {
					case violation <- r.Method:
					default:
					}
				}
			default:
			}
		}
		return f.h.handle(r)
	})
	f.svc.Herdr = herdr.Client{SocketPath: server.Path}
	retireDone := make(chan error, 1)
	go func() {
		_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: spare})
		retireDone <- err
	}()
	select {
	case <-inside:
	case <-time.After(5 * time.Second):
		t.Fatal("Retire did not reach its locked close")
	}

	type answer struct {
		res workforce.HandoffAckResult
		err error
	}
	ackStarted, ackDone := make(chan struct{}), make(chan answer, 1)
	go func() {
		close(ackStarted) // Explicit launch barrier before attempting the op.
		res, err := f.svc.HandoffAck(t.Context(), workforce.HandoffAckRequest{Target: next})
		ackDone <- answer{res, err}
	}()
	select {
	case <-ackStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("ack goroutine did not start")
	}
	window := time.NewTimer(time.Second)
	defer window.Stop()
	select {
	case method := <-violation:
		assert.Failf(t, "ack crossed Retire's workforce lock", "%s before Retire was released", method)
	case <-window.C:
	}
	assert.Equal(t, before, f.row(t, w.ID), "no rebind while Retire holds the lock")
	assert.Equal(t, "pending", handoffState(t, f, h))
	assert.Equal(t, w.ID, f.nameOf(w.PaneID))
	assert.Equal(t, w.ID+"-next", f.nameOf(next))
	assert.Empty(t, rec.all(), "no completion while Retire holds the lock")

	unblock()
	select {
	case err := <-retireDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Retire did not finish after release")
	}
	select {
	case a := <-ackDone:
		require.NoError(t, a.err)
		assert.True(t, a.res.Changed)
	case <-time.After(5 * time.Second):
		t.Fatal("ack did not finish after Retire released the lock")
	}
	assert.Equal(t, "ended", handoffState(t, f, h))
	assert.Equal(t, next, f.row(t, w.ID).PaneID)
	assert.Len(t, rec.all(), 1, "ack proceeds and completes once after release")
}
