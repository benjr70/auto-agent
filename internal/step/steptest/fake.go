// Package steptest provides a fake `claude` binary: a script that replays a
// scripted stream and records how it was called, so the Step runner and
// everything above it are tested with no model call.
package steptest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// TB is the part of *testing.T the fake uses.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	TempDir() string
}

// Fake is a fake claude binary. Point the runner at Path.
type Fake struct {
	// Path is the executable to run in place of claude.
	Path string

	t   TB
	dir string
}

// Reply is what one call of the fake does.
type Reply struct {
	// Lines are written to stdout, one per line: the stream.
	Lines []string
	// Stderr is written to stderr.
	Stderr string
	// ExitCode is the exit status.
	ExitCode int
	// Run is shell the fake runs in its working directory before it
	// replies: what a Step's edits to the checkout look like.
	Run string
	// Hang makes the fake start a child that holds stdout open and never
	// exits, then wait on it: what a stuck tool call looks like.
	Hang bool
}

// Call is one recorded call of the fake.
type Call struct {
	Args []string
	Env  map[string]string
	// Stdin is what the call was given on standard input: the prompt.
	Stdin string
	// Dir is the working directory.
	Dir string
	// Plugin holds the files under --plugin-dir at the time of the call,
	// by path relative to it.
	Plugin map[string]string
	// ChildPID is the process the fake left holding stdout, when it hung.
	ChildPID int
}

// Arg is the value following flag in the call's arguments, and whether the
// flag was passed at all.
func (c Call) Arg(flag string) (string, bool) {
	for i, a := range c.Args {
		if a == flag {
			if i+1 < len(c.Args) {
				return c.Args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

const script = `#!/bin/sh
# A fake claude for tests: replay the scripted reply, record the call.
here="$(cd "$(dirname "$0")" && pwd)"
n=$(( $(cat "$here/count" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$here/count"
call="$here/call-$n"
mkdir -p "$call"
: > "$call/args"
plugin=""
prev=""
for a in "$@"; do
    printf '%s\0' "$a" >> "$call/args"
    [ "$prev" = "--plugin-dir" ] && plugin="$a"
    prev="$a"
done
cat > "$call/stdin"
env > "$call/env"
pwd > "$call/dir"
[ -n "$plugin" ] && [ -d "$plugin" ] && cp -R "$plugin" "$call/plugin"
reply="$here/reply-$n"
[ -d "$reply" ] || reply="$here/reply-$(cat "$here/last")"
[ -f "$reply/run" ] && sh "$reply/run" >/dev/null
cat "$reply/stdout"
cat "$reply/stderr" >&2
if [ -f "$reply/hang" ]; then
    sleep 600 &
    echo $! > "$call/child"
    wait
fi
exit "$(cat "$reply/exit")"
`

// New writes a fake claude into a temporary directory. With no reply
// scripted it prints nothing and exits 0.
func New(t TB) *Fake {
	t.Helper()
	f := &Fake{t: t, dir: t.TempDir()}
	f.Path = filepath.Join(f.dir, "claude")
	f.write(f.Path, script, 0o755)
	f.Script(Reply{})
	return f
}

// Script sets the replies to the calls still to come, replacing any scripted
// before. The last one repeats.
func (f *Fake) Script(replies ...Reply) {
	f.t.Helper()
	if len(replies) == 0 {
		f.t.Fatalf("steptest: Script needs at least one reply")
	}
	stale, _ := filepath.Glob(filepath.Join(f.dir, "reply-*"))
	for _, dir := range stale {
		os.RemoveAll(dir)
	}
	made := 0
	if count, err := os.ReadFile(filepath.Join(f.dir, "count")); err == nil {
		made, _ = strconv.Atoi(strings.TrimSpace(string(count)))
	}
	for i, r := range replies {
		dir := filepath.Join(f.dir, "reply-"+strconv.Itoa(made+i+1))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			f.t.Fatalf("steptest: %v", err)
		}
		stdout := ""
		if len(r.Lines) > 0 {
			stdout = strings.Join(r.Lines, "\n") + "\n"
		}
		f.write(filepath.Join(dir, "stdout"), stdout, 0o644)
		f.write(filepath.Join(dir, "stderr"), r.Stderr, 0o644)
		f.write(filepath.Join(dir, "exit"), strconv.Itoa(r.ExitCode), 0o644)
		if r.Run != "" {
			f.write(filepath.Join(dir, "run"), r.Run, 0o644)
		}
		if r.Hang {
			f.write(filepath.Join(dir, "hang"), "", 0o644)
		}
	}
	f.write(filepath.Join(f.dir, "last"), strconv.Itoa(made+len(replies)), 0o644)
}

func (f *Fake) write(path, content string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		f.t.Fatalf("steptest: %v", err)
	}
}

// Calls are the calls made so far, in order.
func (f *Fake) Calls() []Call {
	f.t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(f.dir, "call-*"))
	sort.Slice(dirs, func(a, b int) bool { return callNumber(dirs[a]) < callNumber(dirs[b]) })
	var calls []Call
	for _, dir := range dirs {
		call := Call{Env: map[string]string{}, Plugin: map[string]string{}}
		if args, err := os.ReadFile(filepath.Join(dir, "args")); err == nil && len(args) > 0 {
			call.Args = strings.Split(strings.TrimSuffix(string(args), "\x00"), "\x00")
		}
		if env, err := os.ReadFile(filepath.Join(dir, "env")); err == nil {
			for _, line := range strings.Split(string(env), "\n") {
				if k, v, ok := strings.Cut(line, "="); ok {
					call.Env[k] = v
				}
			}
		}
		if stdin, err := os.ReadFile(filepath.Join(dir, "stdin")); err == nil {
			call.Stdin = string(stdin)
		}
		if wd, err := os.ReadFile(filepath.Join(dir, "dir")); err == nil {
			call.Dir = strings.TrimSpace(string(wd))
		}
		if pid, err := os.ReadFile(filepath.Join(dir, "child")); err == nil {
			call.ChildPID, _ = strconv.Atoi(strings.TrimSpace(string(pid)))
		}
		root := filepath.Join(dir, "plugin")
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.Mode().IsRegular() {
				rel, _ := filepath.Rel(root, path)
				data, _ := os.ReadFile(path)
				call.Plugin[rel] = string(data)
			}
			return nil
		})
		calls = append(calls, call)
	}
	return calls
}

func callNumber(dir string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), "call-"))
	return n
}

// The stream lines Claude Code prints, as far as the runner reads them.

// Init is the init message: the session, the model, the tools the call can
// reach and the slash commands that loaded.
func Init(session, model string, tools, slashCommands []string) string {
	return line(map[string]any{
		"type": "system", "subtype": "init", "session_id": session, "model": model,
		"permissionMode": "dontAsk", "tools": tools, "slash_commands": slashCommands,
	})
}

// RateLimit is a rate-limit event with the given status ("allowed",
// "allowed_warning", "rejected").
func RateLimit(status string, resetsAt int64, limitType string) string {
	return line(map[string]any{
		"type":            "rate_limit_event",
		"rate_limit_info": map[string]any{"status": status, "resetsAt": resetsAt, "rateLimitType": limitType},
	})
}

// Success is a successful result carrying structured output.
func Success(structuredOutput any) string {
	return Result(map[string]any{"subtype": "success", "is_error": false, "result": "done", "structured_output": structuredOutput})
}

// Result is a result message with the given fields over the usual ones.
func Result(fields map[string]any) string {
	result := map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "",
		"num_turns": 1, "total_cost_usd": 0.0, "usage": map[string]any{}, "permission_denials": []any{},
	}
	for k, v := range fields {
		result[k] = v
	}
	return line(result)
}

func line(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("steptest: %v", err))
	}
	return string(b)
}
