//go:build unix

package report_test

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/report"
)

// TestTranscriptContextRefusesAFIFO: a FIFO at transcript_path is refused at
// once. Opened for reading the ordinary way it would wait for a writer that
// never comes, holding the hook until Codex kills it.
func TestTranscriptContextRefusesAFIFO(t *testing.T) {
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	_, err := report.TranscriptContext(fifo)
	require.ErrorIs(t, err, report.ErrMalformed)
}
