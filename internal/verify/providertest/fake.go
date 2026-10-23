// Package providertest provides a fake Environment provider: an executable
// that answers up, down, smoke and status as scripted and records how it was called,
// so the driving of the contract is tested with no environment to boot.
package providertest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// TB is the part of *testing.T the fake uses.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	TempDir() string
}

// Reply is what one call of a subcommand does.
type Reply struct {
	Stdout, Stderr string
	Exit           int
	// Run is shell the fake runs in its working directory first: what a
	// provider leaving files in the checkout looks like.
	Run string
}

// Script is what each subcommand answers, call by call; the last reply of
// each repeats. A subcommand with no reply exits 0 and prints nothing.
type Script struct {
	Up, Down, Smoke []Reply
	// Status is the deployed tier's command answering status.
	Status []Reply
}

// Healthy is a provider that boots, passes its smoke and tears down.
func Healthy(block string) Script {
	return Script{Up: []Reply{{Stdout: block}}, Smoke: []Reply{{Stdout: "checking...\nsmoke: PASS (2/2)\n"}}}
}

// Fake is a fake Environment provider. Point the hermetic command at Path.
type Fake struct {
	Path string
	t    TB
	dir  string
}

const script = `#!/bin/sh
# A fake Environment provider for tests: replay the scripted reply, record
# the call.
here="$(cd "$(dirname "$0")/.provider" && pwd)"
printf '%s\n' "$*" >> "$here/calls"
cmd="$1"
n=$(( $(cat "$here/count-$cmd" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$here/count-$cmd"
[ "$cmd" = smoke ] && env > "$here/smoke-env"
pwd > "$here/dir"
reply="$here/$cmd-$n"
[ -d "$reply" ] || reply="$here/$cmd-last"
[ -f "$reply/run" ] && sh "$reply/run" >/dev/null 2>&1
[ -f "$reply/stdout" ] && cat "$reply/stdout"
[ -f "$reply/stderr" ] && cat "$reply/stderr" >&2
exit "$(cat "$reply/exit" 2>/dev/null || echo 0)"
`

// New writes a fake provider, answering as script says, into a temporary
// directory.
func New(t TB, s Script) *Fake {
	t.Helper()
	f := &Fake{t: t, dir: t.TempDir()}
	f.Path = filepath.Join(f.dir, "provider")
	f.write(f.Path, script, 0o755)
	f.Script(s)
	return f
}

func (f *Fake) state() string { return filepath.Join(f.dir, ".provider") }

// Script replaces the replies, and starts every subcommand's count again.
func (f *Fake) Script(s Script) {
	f.t.Helper()
	os.RemoveAll(f.state())
	for cmd, replies := range map[string][]Reply{"up": s.Up, "down": s.Down, "smoke": s.Smoke, "status": s.Status} {
		for i, r := range replies {
			f.reply(cmd+"-"+strconv.Itoa(i+1), r)
			if i == len(replies)-1 {
				f.reply(cmd+"-last", r)
			}
		}
	}
	os.MkdirAll(f.state(), 0o755)
}

func (f *Fake) reply(name string, r Reply) {
	dir := filepath.Join(f.state(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatalf("providertest: %v", err)
	}
	f.write(filepath.Join(dir, "stdout"), r.Stdout, 0o644)
	f.write(filepath.Join(dir, "stderr"), r.Stderr, 0o644)
	f.write(filepath.Join(dir, "exit"), strconv.Itoa(r.Exit), 0o644)
	if r.Run != "" {
		f.write(filepath.Join(dir, "run"), r.Run, 0o644)
	}
}

func (f *Fake) write(path, content string, mode os.FileMode) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		f.t.Fatalf("providertest: %v", err)
	}
}

// Calls are the calls made so far, as their arguments: "down --pr 5",
// "up --pr 5", "smoke".
func (f *Fake) Calls() []string {
	data, _ := os.ReadFile(filepath.Join(f.state(), "calls"))
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// SmokeEnv is the value of key in the environment the last smoke ran with,
// and whether it was set.
func (f *Fake) SmokeEnv(key string) (string, bool) {
	data, _ := os.ReadFile(filepath.Join(f.state(), "smoke-env"))
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// Dir is the working directory of the last call.
func (f *Fake) Dir() string {
	data, _ := os.ReadFile(filepath.Join(f.state(), "dir"))
	return strings.TrimSpace(string(data))
}
