// Package bundletest is a stand-in for the Agentisan bundle's MCP server in
// tests: the test binary itself, run again in a mode, answering with the
// replies captured from the real 0.19.0 server (testdata, machine paths
// replaced by /repo). No test starts uv, Python or the real bundle.
package bundletest

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/abanna/herdr-agentisan/internal/bundle"
)

const (
	contentKey = "content"
	isErrorKey = "isError"
)

//go:embed testdata/*.json
var replies embed.FS

// Environment variables of the stand-in. Mode picks how it behaves; the
// others refine a mode.
const (
	ModeEnv = "BUNDLETEST_MODE"
	// CodeEnv is the bundle error code the "envelope" mode answers with.
	CodeEnv = "BUNDLETEST_CODE"
	// BadEnv is the fault the "bad_reply" mode injects.
	BadEnv = "BUNDLETEST_BAD"
	// StageEnv is the stage the read-state reply carries, when set.
	StageEnv = "BUNDLETEST_STAGE"
	// PidFileEnv gives the file where the stand-in records its child PID.
	PidFileEnv = "BUNDLETEST_PIDFILE"
	// WorkDirEnv is the expected work_dir argument, if set.
	WorkDirEnv = "BUNDLETEST_WORKDIR"
)

// Active reports whether this process is the stand-in.
func Active() bool { return os.Getenv(ModeEnv) != "" }

// Main serves on stdin and stdout and exits. It never returns.
func Main() {
	os.Exit(serve(os.Stdin, os.Stdout, os.Getenv))
}

// Launch returns the Launch of a stand-in in mode, started in repo, with
// extra KEY=VALUE variables.
func Launch(t testing.TB, repo, mode string, extra ...string) bundle.Launch {
	t.Helper()
	env := append(os.Environ(), ModeEnv+"="+mode)
	return bundle.Launch{Argv: []string{os.Args[0]}, Dir: repo, Env: append(env, extra...), ClientVersion: "test"}
}

func fixture(name string) map[string]any {
	raw, err := replies.ReadFile("testdata/" + name + ".json")
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		panic(err)
	}
	return m
}

// textItem is one text block of a tool result.
func textItem(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// toolResult is a tools/call result carrying env as both representations.
func toolResult(env map[string]any, isError, structured bool) map[string]any {
	text, _ := json.MarshalIndent(env, "", "  ")
	r := map[string]any{contentKey: []any{textItem(string(text))}, isErrorKey: isError}
	if structured {
		r["structuredContent"] = env
	}
	return r
}

// readState is the real read-state envelope, with the stage and the
// faults the environment asks for.
func readState(getenv func(string) string) map[string]any {
	env := fixture("read-state")["result"].(map[string]any)["structuredContent"].(map[string]any)
	state := env["result"].(map[string]any)["state"].(map[string]any)
	if s := getenv(StageEnv); s != "" {
		state["stage"] = s
	}
	switch getenv(BadEnv) {
	case "no_updated_at":
		delete(state, "updated_at")
	case "bad_stage":
		state["stage"] = 7
	case "wrong_op":
		env["operation_id"] = "convergence.update_state"
	}
	return env
}

func serve(in io.Reader, out *os.File, getenv func(string) string) int {
	mode := getenv(ModeEnv)
	if mode == "sleep" {
		time.Sleep(10 * time.Second)
		return 0
	}
	if path := getenv(PidFileEnv); path != "" {
		if err := startChild(out, path); err != nil {
			return 8
		}
	}
	enc := json.NewEncoder(out)
	send := func(id json.RawMessage, result map[string]any) {
		frame := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
		if mode == "at_line_limit" && string(id) == "2" {
			body, _ := json.Marshal(frame)
			_, _ = out.Write(body)
			_, _ = io.WriteString(out, strings.Repeat(" ", (4<<20)-len(body)-1)+"\n")
			return
		}
		_ = enc.Encode(frame)
	}
	rd := bufio.NewReader(in)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			if mode == "stubborn" {
				time.Sleep(time.Hour)
			}
			return 0
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments struct {
					OperationID string `json:"operation_id"`
					Values      struct {
						WorkDir string `json:"work_dir"`
					} `json:"arguments"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 {
			continue
		}
		if req.Method == "initialize" {
			if code, done := initialize(mode, send, req.ID); done {
				return code
			}
			continue
		}
		if want := getenv(WorkDirEnv); want != "" && req.Params.Arguments.Values.WorkDir != want {
			return 9
		}
		if code, done := call(mode, getenv, out, send, req.ID, req.Params.Arguments.OperationID); done {
			return code
		}
	}
}

func initialize(mode string, send func(json.RawMessage, map[string]any), id json.RawMessage) (int, bool) {
	res := fixture("initialize")["result"].(map[string]any)
	info := res["serverInfo"].(map[string]any)
	switch mode {
	case "silent_start":
		time.Sleep(time.Hour)
	case "early_exit":
		return 3, true
	case "old_version":
		info["version"] = "0.18.9"
	case "bad_version":
		info["version"] = "unknown"
	case "wrong_protocol":
		res["protocolVersion"] = "2024-11-05"
	case "wrong_name":
		info["name"] = "other"
	case "no_tools":
		delete(res["capabilities"].(map[string]any), "tools")
	}
	send(id, res)
	return 0, false
}

func call(mode string, getenv func(string) string, out io.Writer, send func(json.RawMessage, map[string]any), id json.RawMessage, op string) (int, bool) {
	envelope := func(code string) map[string]any {
		return map[string]any{"status": "error", "operation_id": op, "error": map[string]any{"code": code, "message": "text of " + code, "details": map[string]any{}}}
	}
	switch mode {
	case "envelope":
		send(id, toolResult(envelope(getenv(CodeEnv)), true, true))
	case "envelope_text":
		send(id, toolResult(envelope(getenv(CodeEnv)), true, false))
	case "rpc_error":
		_, _ = fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"bad params"}}`+"\n", id)
	case "tool_text":
		send(id, map[string]any{contentKey: []any{textItem("Error executing tool execute: boom")}, isErrorKey: true})
	case "die_mid_call":
		return 7, true
	case "oversize_line":
		_, _ = io.WriteString(out, strings.Repeat(" ", (4<<20)+1))
		time.Sleep(time.Hour)
	case "hang_call":
		time.Sleep(time.Hour)
	case "bad_reply":
		bad(getenv(BadEnv), getenv, out, send, id, op)
	default:
		if op != "convergence.read_state" {
			send(id, toolResult(fixture("unknown-op")["result"].(map[string]any)["structuredContent"].(map[string]any), true, true))
			return 0, false
		}
		send(id, toolResult(readState(getenv), false, mode != "text_only"))
	}
	return 0, false
}

// bad answers with one malformed reply.
func bad(kind string, getenv func(string) string, out io.Writer, send func(json.RawMessage, map[string]any), id json.RawMessage, _ string) {
	switch kind {
	case "garbage":
		_, _ = fmt.Fprintln(out, "{not json")
	case "wrong_id":
		send(json.RawMessage("99"), toolResult(readState(getenv), false, true))
	case "server_request":
		_, _ = fmt.Fprintln(out, `{"jsonrpc":"2.0","id":"s1","method":"ping"}`)
	case "no_payload":
		send(id, map[string]any{contentKey: []any{}, isErrorKey: false})
	default:
		send(id, toolResult(readState(getenv), false, true))
	}
}

// startChild gives a sleeping child this server's stdout, so the pipe
// stays open until both processes exit.
func startChild(out *os.File, path string) error {
	child := exec.CommandContext(context.Background(), os.Args[0]) // #nosec G204 G702 -- the test binary again
	child.Env = append(os.Environ(), ModeEnv+"=sleep")
	child.Stdout = out
	if err := child.Start(); err != nil {
		return fmt.Errorf("start the stand-in child: %w", err)
	}
	if err := os.WriteFile(path, fmt.Appendf(nil, "%d", child.Process.Pid), 0o600); err != nil { // #nosec G703 -- the test names the file
		return fmt.Errorf("record the stand-in child: %w", err)
	}
	return nil
}
