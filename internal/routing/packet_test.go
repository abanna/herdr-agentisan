package routing_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
)

// TestDecodeTask: a packet is one JSON object of the rules' task_packet
// keys, read whole.
func TestDecodeTask(t *testing.T) {
	t.Parallel()
	got, err := routing.DecodeTask(strings.NewReader(`{"task_id":"NERD-5289","task_lookup_status":"found","title":"routing 1b",
	"requirements":"wire spawn","acceptance_criteria":["one decision"],"worker_assignment":"spawn.go","requested_role":"coder",
	"provider_preference":"claude","required_tools":["go"],"previous_attempts":"","user_model_override":null}`))
	require.NoError(t, err)
	id := "NERD-5289"
	assert.Equal(t, &routing.Task{
		TaskID: &id, LookupStatus: routing.LookupFound, Title: "routing 1b", Requirements: "wire spawn", AcceptanceCriteria: []string{"one decision"},
		WorkerAssignment: "spawn.go", RequestedRole: "coder", ProviderPreference: "claude", RequiredTools: []string{"go"},
	}, got)

	for name, in := range map[string]string{
		"an unknown key":             `{"task_lookup_status":"found","priority":1}`,
		"a mistyped key":             `{"task_lookup_status":"found","acceptance_criteria":"one"}`,
		"two objects":                `{"task_lookup_status":"found"} {"task_lookup_status":"found"}`,
		"trailing text":              `{"task_lookup_status":"found"} x`,
		"no object":                  ``,
		"bytes that are not UTF-8":   "{\"task_lookup_status\":\"found\",\"worker_assignment\":\"\xff\"}",
		"a surrogate's bytes":        "{\"task_lookup_status\":\"found\",\"worker_assignment\":\"\xed\xa0\x80\"}",
		"a BOM first":                "\ufeff{\"task_lookup_status\":\"found\"}",
		"an array":                   `[]`,
		"a file a byte past 64 KiB":  `{"title":"x"` + strings.Repeat(" ", routing.MaxTaskBytes-12) + `}`,
		"a field past its own bound": `{"title":"` + strings.Repeat("x", 513) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := routing.DecodeTask(strings.NewReader(in))
			require.ErrorIs(t, err, routing.ErrInvalid)
		})
	}
	// Keys match as encoding/json matches them, in any case; an escaped lone
	// surrogate is JSON text, which decodes to U+FFFD.
	got, err = routing.DecodeTask(strings.NewReader(`{"TASK_LOOKUP_STATUS":"found","worker_assignment":"\ud800"}`))
	require.NoError(t, err)
	assert.Equal(t, &routing.Task{LookupStatus: routing.LookupFound, WorkerAssignment: "\ufffd"}, got)
	// A whole object padded to 64 KiB exactly, its text well in bounds.
	got, err = routing.DecodeTask(strings.NewReader(`{"title":"x"` + strings.Repeat(" ", routing.MaxTaskBytes-13) + `}`))
	require.NoError(t, err)
	assert.Equal(t, "x", got.Title)
	_, err = routing.DecodeTask(iotest.ErrReader(fmt.Errorf("disk gone")))
	require.ErrorIs(t, err, routing.ErrInvalid)
	assert.Contains(t, err.Error(), "disk gone")
}

// TestTaskBounds: each field takes up to its bound and no more, a list up
// to 64 items of 2 KiB, and all the text together up to 64 KiB, whichever
// way the packet arrived.
func TestTaskBounds(t *testing.T) {
	t.Parallel()
	x := strings.Repeat
	id := func(n int) *string { s := x("x", n); return &s }
	items := func(n, each int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = x("x", each)
		}
		return out
	}
	tests := map[string]struct {
		task routing.Task
		bad  string
	}{
		"task_id at 512":                {task: routing.Task{TaskID: id(512)}},
		"task_id past 512":              {task: routing.Task{TaskID: id(513)}, bad: "task_id"},
		"title past 512":                {task: routing.Task{Title: x("x", 513)}, bad: "title"},
		"requirements at 16 KiB":        {task: routing.Task{Requirements: x("x", 16<<10)}},
		"requirements past 16 KiB":      {task: routing.Task{Requirements: x("x", 16<<10+1)}, bad: "requirements"},
		"worker_assignment past 16 KiB": {task: routing.Task{WorkerAssignment: x("x", 16<<10+1)}, bad: "worker_assignment"},
		"previous_attempts past 16 KiB": {task: routing.Task{PreviousAttempts: x("x", 16<<10+1)}, bad: "previous_attempts"},
		"64 criteria":                   {task: routing.Task{AcceptanceCriteria: items(64, 1), RequiredTools: items(64, 1)}},
		"criteria at 2 KiB":             {task: routing.Task{AcceptanceCriteria: items(16, 2<<10), RequiredTools: items(1, 2<<10)}},
		"65 criteria":                   {task: routing.Task{AcceptanceCriteria: items(65, 1)}, bad: "acceptance_criteria"},
		"a criterion past 2 KiB":        {task: routing.Task{AcceptanceCriteria: items(1, 2<<10+1)}, bad: "acceptance_criteria[0]"},
		"65 tools":                      {task: routing.Task{RequiredTools: items(65, 1)}, bad: "required_tools"},
		"a tool past 2 KiB":             {task: routing.Task{RequiredTools: []string{"go", x("x", 2<<10+1)}}, bad: "required_tools[1]"},
		"all the text at 64 KiB": {task: routing.Task{
			Requirements: x("x", 16<<10), WorkerAssignment: x("x", 16<<10), PreviousAttempts: x("x", 16<<10), AcceptanceCriteria: items(8, 2<<10),
		}},
		"all the text past 64 KiB": {task: routing.Task{
			Requirements: x("x", 16<<10), WorkerAssignment: x("x", 16<<10), PreviousAttempts: x("x", 16<<10), AcceptanceCriteria: items(8, 2<<10),
			LookupStatus: "x",
		}, bad: "text is over 65536 bytes"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.task.CheckBounds()
			if tc.bad == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, routing.ErrInvalid)
			assert.Contains(t, err.Error(), tc.bad)
			raw, err := json.Marshal(tc.task)
			require.NoError(t, err)
			_, err = routing.DecodeTask(strings.NewReader(string(raw)))
			require.ErrorIs(t, err, routing.ErrInvalid, "decoded, too")
		})
	}
}
