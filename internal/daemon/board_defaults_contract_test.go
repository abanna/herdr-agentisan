package daemon_test

import (
	"os"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestBoardDefaultsMatchA60 pins the documented policy independently of
// the constants used by the existing board behavior tests.
func TestBoardDefaultsMatchA60(t *testing.T) {
	t.Parallel()

	t.Run("wait", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 30*time.Minute, daemon.DefaultBoardWait, "A60 default wait")
	})
	t.Run("limit", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 50, daemon.DefaultBoardLimit, "A60 default read limit")
	})
	t.Run("retention", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 30*24*time.Hour, store.BoardRetention, "A60 retention")
	})
}

// TestBoardDefaultsAreWhatTheDaemonUses pins the three A60 defaults where the
// daemon applies them, by their effect, each as a literal of its own: a read
// given no limit, a wait given no timeout, and the prune the daemon runs.
// TestBoardDefaultsMatchA60 pins the constants; this fails when the code
// stops using them, or uses them off by one.
func TestBoardDefaultsAreWhatTheDaemonUses(t *testing.T) {
	t.Parallel()

	t.Run("limit", func(t *testing.T) {
		t.Parallel()
		st := boardStore(t)
		for i := range 51 {
			boardPost(t, st, "ha", "post "+strconv.Itoa(i))
		}
		socket, _, _ := daemon.ServeBoard(t, shortDir(t), st, as(theBoss))

		got, err := boardOp[daemon.BoardReadResult](t, socket, daemon.OpBoardRead, daemon.BoardReadRequest{})
		require.NoError(t, err)
		require.Len(t, got.Posts, 50, "a read given no limit returns 50 of 51 unread posts")
		assert.Equal(t, "post 0", got.Posts[0].Body, "oldest first")
		assert.Equal(t, "post 49", got.Posts[49].Body)
		assert.Equal(t, 1, got.More, "the 51st stays unread")

		all, err := boardOp[daemon.BoardReadResult](t, socket, daemon.OpBoardRead, daemon.BoardReadRequest{All: true})
		require.NoError(t, err)
		require.Len(t, all.Posts, 50, "--all given no limit returns the newest 50")
		assert.Equal(t, "post 50", all.Posts[0].Body, "newest first")
		assert.Equal(t, "post 1", all.Posts[49].Body)
	})

	t.Run("wait", func(t *testing.T) {
		t.Parallel()
		// The bubble's clock is fake, so the 30 minutes pass at once, and
		// the store, whose channels the wait blocks on, opens inside it.
		synctest.Test(t, func(t *testing.T) {
			st := boardStore(t)
			type outcome struct {
				res daemon.BoardStateResult
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				res, err := daemon.WaitOn(t.Context(), st, as(theBoss), daemon.BoardWaitRequest{})
				done <- outcome{res, err}
			}()
			synctest.Wait() // the wait has started its timer and blocks on the board

			time.Sleep(30*time.Minute - time.Nanosecond)
			synctest.Wait()
			select {
			case o := <-done:
				t.Fatalf("a wait given no timeout ended before 30 minutes: %+v, %v", o.res, o.err)
			default:
			}

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case o := <-done:
				require.NoError(t, o.err)
				assert.True(t, o.res.TimedOut, "a wait given no timeout ends at 30 minutes, timed out")
			default:
				t.Fatal("a wait given no timeout had not ended at 30 minutes")
			}
		})
	})

	t.Run("retention", func(t *testing.T) {
		t.Parallel()
		t0 := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
		h := newHerdr(t, 22)
		dir := shortDir(t)
		p := paths(t, dir, h)
		require.NoError(t, os.MkdirAll(p.Dir, 0o700))
		st, err := store.Open(t.Context(), p.DB)
		require.NoError(t, err)
		// A post exactly 30 days old stays; one a nanosecond older goes.
		for body, at := range map[string]time.Time{
			"expired": t0.Add(-30*24*time.Hour - time.Nanosecond),
			"kept":    t0.Add(-30 * 24 * time.Hour),
		} {
			_, err := st.Post(t.Context(), store.BoardPost{Project: "ha", Channel: store.ChannelTeam, Author: store.Address{Kind: store.KindWorker, ID: "w"}, Kind: "post", Body: body, At: at})
			require.NoError(t, err)
		}
		require.NoError(t, st.Close())

		o := opts(t, dir, h)
		o.Now = func() time.Time { return t0 }
		serving(t, o, h)
		require.Eventually(t, func() bool {
			return assert.ObjectsAreEqual([]string{"kept"}, query(t, p.DB, "SELECT body FROM board_posts ORDER BY seq"))
		}, ready, 10*time.Millisecond, "the daemon's prune keeps a post 30 days old and removes one a nanosecond older")
	})
}
