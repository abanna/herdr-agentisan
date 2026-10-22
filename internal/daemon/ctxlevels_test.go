package daemon_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
)

// toasts are the notification.show params th received, in order.
func toasts(t *testing.T, th *teamHerdr) []herdr.Notification {
	t.Helper()
	th.srv.Settle(t)
	var out []herdr.Notification
	for _, r := range th.srv.Requests() {
		if r.Method == "notification.show" {
			var n herdr.Notification
			require.NoError(t, json.Unmarshal(r.Params, &n))
			out = append(out, n)
		}
	}
	return out
}

// ctxRun runs a daemon with o, its log in logs as daemon.log has it, and
// returns it with its events.
func ctxRun(t *testing.T, o daemon.Options, logs *lockedLog) (*running, *telemetrytest.Recorder) {
	t.Helper()
	logger, err := logging.New(logs, "info", "json")
	require.NoError(t, err)
	o.Logger = logger
	events, rec := telemetrytest.NewEvents()
	o.Team.Events = events
	return run(t, o), rec
}

// ctxEvents are rec's ctx level events, each as its worker, level, ctx,
// alerted and shown.
func ctxEvents(rec *telemetrytest.Recorder) []string {
	var out []string
	for _, e := range rec.Events() {
		if e.Name != telemetry.EventCtxLevel {
			continue
		}
		a := e.Attrs
		out = append(out, strings.Join([]string{
			a["agentisan.worker.id"].(string), a["agentisan.role"].(string), a["agentisan.ctx.level"].(string),
			jsonOf(a["agentisan.ctx.percent"]), jsonOf(a["agentisan.ctx.threshold"]), jsonOf(a["agentisan.ctx.alerted"]), jsonOf(a["agentisan.ctx.shown"]),
		}, " "))
	}
	return out
}

func jsonOf(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// TestCtxLevelsReachTheBoss is A33's notices end to end on herdrtest: a
// worker's ctx crossing each level is told to its boss once, the ceiling
// after a toast that alerts the user, once; the boss is told about its own
// context; a pane in no team is never told about; each crossing is an
// event; and no poll makes a herdr call it did not make before, but the
// toast.
func TestCtxLevelsReachTheBoss(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setAgents(named("wJ:p1", "boss"), named("wN:p1", "pee01"))
	th.setToken("w2:p1", "ctx", "90")
	var logs lockedLog
	_, rec := ctxRun(t, teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n")), &logs)

	waitLine(t, &logs, "the daemon started")
	th.setToken("wN:p1", "ctx", "56")
	waitLine(t, &logs, "pee01 is at 56% context (soft: 55%): let it finish its unit, then rotate it")
	th.setToken("wN:p1", "ctx", "66")
	waitLine(t, &logs, "pee01 is at 66% context (hard: 65%): rotate it now")
	assert.Empty(t, toasts(t, th), "only the ceiling alerts the user")
	before := methodsOf(t, th)
	th.setToken("wN:p1", "ctx", "76")
	waitLine(t, &logs, "pee01 is at 76% context (ceiling: 75%): act now; the user has been alerted")
	th.setToken("wJ:p1", "ctx", "80")
	waitLine(t, &logs, "you are at 80% context (ceiling: 75%): hand off now; the user has been alerted")
	polled(t, th.srv, 3)

	lines := slices.DeleteFunc(noticeLines(&logs), func(l string) bool { return !strings.Contains(l, "context") })
	assert.Equal(t, []string{
		"pee01 is at 56% context (soft: 55%): let it finish its unit, then rotate it",
		"pee01 is at 66% context (hard: 65%): rotate it now",
		"pee01 is at 76% context (ceiling: 75%): act now; the user has been alerted",
		"you are at 80% context (ceiling: 75%): hand off now; the user has been alerted",
	}, lines, "each once; w2:p1, in no team, never")
	assert.Equal(t, []herdr.Notification{
		{Title: "agentisan: pee01 at 76% context", Body: "past the 75% ceiling; its boss is told to rotate it", Sound: "request"},
		{Title: "agentisan: boss at 80% context", Body: "the boss is past the 75% ceiling and is told to hand off", Sound: "request"},
	}, toasts(t, th), "one toast per ceiling crossed")
	assert.Equal(t, []string{
		"wN:p1 worker soft 56 55 false false",
		"wN:p1 worker hard 66 65 false false",
		"wN:p1 worker ceiling 76 75 true true",
		"wJ:p1 boss ceiling 80 75 true true",
	}, ctxEvents(rec))

	after := methodsOf(t, th)
	for m := range before {
		delete(after, m)
	}
	assert.Equal(t, map[string]bool{"notification.show": true}, after, "the ceilings add the toast to the poll's calls, and nothing else")
}

// methodsOf is every method th has been asked so far.
func methodsOf(t *testing.T, th *teamHerdr) map[string]bool {
	t.Helper()
	th.srv.Settle(t)
	out := map[string]bool{}
	for _, r := range th.srv.Requests() {
		out[r.Method] = true
	}
	return out
}

// TestCtxCeilingToastNotShown: a toast herdr does not show, a call that
// fails, and one that takes past ToastBudget are each logged and never asked
// again, and the boss's line says the user was not alerted.
func TestCtxCeilingToastNotShown(t *testing.T) {
	t.Parallel()
	for name, toast := range map[string]herdrtest.Handler{
		"not shown": func(herdrtest.Request) herdrtest.Reply {
			return herdrtest.Reply{Result: map[string]any{"type": "notification_show", "shown": false, "reason": "no_foreground_client"}}
		},
		"refused": func(herdrtest.Request) herdrtest.Reply {
			return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "server_unavailable", Message: "busy"}}
		},
		"past its budget": func(herdrtest.Request) herdrtest.Reply {
			time.Sleep(2 * daemon.ToastBudget)
			return herdrtest.Reply{Result: map[string]any{"type": "notification_show", "shown": true, "reason": ""}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			th.setAgents(named("wN:p1", "pee01"))
			th.toast = toast
			var logs lockedLog
			_, rec := ctxRun(t, teamOpts(t, shortDir(t), th, noticesConfig(t, "gap = 0\n")), &logs)

			waitLine(t, &logs, "the daemon started")
			th.setToken("wN:p1", "ctx", "90")
			waitLine(t, &logs, "pee01 is at 90% context (ceiling: 75%): act now; the user's alert was not shown")
			polled(t, th.srv, 3)
			assert.Len(t, toasts(t, th), 1, "never asked again")
			assert.Contains(t, logs.String(), "the context ceiling's toast")
			assert.Equal(t, []string{"wN:p1 worker ceiling 90 75 true false"}, ctxEvents(rec))
		})
	}
}

// TestCtxThresholdsFromTheRepoFile: a group's thresholds come from its
// project's repo file, read when the daemon starts (A46, A33), and a repo
// file that cannot be read is logged and leaves its project the thresholds
// of its other layers.
func TestCtxThresholdsFromTheRepoFile(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		repoFile, want string
	}{
		"its group's":                       {"[groups.coders]\nworkers = 1\nslots = 2\nrotation = { thresholds = [20, 40, 60] }\n", "pee01 is at 40% context (hard: 40%): rotate it now"},
		"not read":                          {"verify = [[\"x\"]]\n", "pee01 is at 40% context (soft: 35%): let it finish its unit, then rotate it"},
		"the project's, from the repo file": {"[rotation]\nthresholds = [30, 35, 38]\n", "pee01 is at 40% context (ceiling: 38%): act now; the user has been alerted"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, cfg := t.TempDir(), t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(repo, settings.RepoFileName), []byte("schema_version = 1\n"+tc.repoFile), 0o600))
			body := "schema_version = 1\n[notices]\ngap = 0\n[spaces]\nproject = \"agentisan\"\n[projects.agentisan]\nrepo = \"" + repo + "\"\n[projects.agentisan.rotation]\nthresholds = [35, 50, 70]\n"
			require.NoError(t, os.WriteFile(filepath.Join(cfg, settings.FileName), []byte(body), 0o600))
			th := newTeamHerdr(t)
			th.setAgents(named("wN:p1", "pee01"))
			var logs lockedLog
			ctxRun(t, teamOpts(t, shortDir(t), th, cfg), &logs)

			waitLine(t, &logs, tc.want)
			if name == "not read" {
				assert.Contains(t, logs.String(), "the project's repo file is not read")
				assert.Contains(t, logs.String(), "verify")
			}
		})
	}
}

// TestCtxThresholdsWhenTheFileDoesNotResolve: a configuration that does
// not resolve is logged, and every team gets the default thresholds.
func TestCtxThresholdsWhenTheFileDoesNotResolve(t *testing.T) {
	t.Parallel()
	cfg := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cfg, settings.FileName), []byte("schema_version = 1\n[rotation]\nthresholds = [1, 2]\n"), 0o600))
	th := newTeamHerdr(t)
	th.setAgents(named("wN:p1", "pee01"))
	th.setToken("wN:p1", "ctx", "56")
	var logs lockedLog
	ctxRun(t, teamOpts(t, shortDir(t), th, cfg), &logs)

	waitLine(t, &logs, "pee01 is at 56% context (soft: 55%)")
	assert.Contains(t, logs.String(), "every team gets the default rotation thresholds")
}

// TestCtxLevelsAfterARestart: a restarted daemon remembers no level, so it
// tells the boss once more about a worker past one.
func TestCtxLevelsAfterARestart(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.setAgents(named("wN:p1", "pee01"))
	th.setToken("wN:p1", "ctx", "70")
	dir, cfg := shortDir(t), noticesConfig(t, "gap = 0\n")
	const line = "pee01 is at 70% context (hard: 65%): rotate it now"

	var before lockedLog
	first, _ := ctxRun(t, teamOpts(t, dir, th, cfg), &before)
	assert.Equal(t, 1, waitLine(t, &before, line))
	polled(t, th.srv, 2)
	first.cancel()
	require.NoError(t, first.wait(t, ready))
	assert.Equal(t, 1, waitLine(t, &before, line), "once while it ran")

	var after lockedLog
	ctxRun(t, restarted(teamOpts(t, dir, th, cfg)), &after)
	assert.Equal(t, 1, waitLine(t, &after, line), "once more after the restart")
}
