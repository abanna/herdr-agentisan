package daemon_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestBoardReadWithoutAcksIsUnchanged (A74, PR 1c): the board's two reads
// now ask board_acks whether each post is acknowledged, and board.read's
// answer carries nothing of it. With no acknowledgement it answers the boss
// and the user byte for byte as before the table, and an acknowledgement
// recorded in the store changes no byte of it either: nothing the daemon
// serves reads it before PR 3c.
func TestBoardReadWithoutAcksIsUnchanged(t *testing.T) {
	t.Parallel()
	st := boardStore(t)
	one := boardPost(t, st, "ha", "one")
	two := boardPost(t, st, "ha", "two")
	view := func(p store.BoardPost) string {
		return fmt.Sprintf(`{"seq":%d,"id":%q,"channel":"team","author_kind":"worker","author":"ha-coder-1","kind":"post","body":%q,"at":"2026-10-23T06:00:00Z"}`, p.Seq, p.ID, p.Body)
	}
	read := func(who daemon.BoardCaller, req daemon.BoardReadRequest) string {
		socket, _, _ := daemon.ServeBoard(t, shortDir(t), st, as(who))
		data, err := daemon.Workforce(t.Context(), socket, daemon.OpBoardRead, req)
		require.NoError(t, err)
		return string(data)
	}

	latest := fmt.Sprintf(`{"project":"ha","reader":"boss","posts":[%s,%s],"more":0,"mark":0,"newest":%d}`, view(two), view(one), two.Seq)
	assert.Equal(t, latest, read(theBoss, daemon.BoardReadRequest{All: true}), "the latest posts, as the panel reads them")
	assert.Equal(t,
		fmt.Sprintf(`{"project":"ha","reader":"user","posts":[%s,%s],"more":0,"mark":%d,"newest":%d}`, view(one), view(two), two.Seq, two.Seq),
		read(theUser, daemon.BoardReadRequest{}), "the user's unread posts")

	_, err := st.AckPost(t.Context(), "ha", one.Seq, boardNow)
	require.NoError(t, err)
	assert.Equal(t, latest, read(theBoss, daemon.BoardReadRequest{All: true}), "an acknowledgement changes no byte of the answer")
	assert.Equal(t,
		fmt.Sprintf(`{"project":"ha","reader":"boss","posts":[%s,%s],"more":0,"mark":%d,"newest":%d}`, view(one), view(two), two.Seq, two.Seq),
		read(theBoss, daemon.BoardReadRequest{}), "the boss's unread posts, one of them acknowledged")
}
