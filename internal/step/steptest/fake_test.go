package steptest

import (
	"os/exec"
	"strings"
	"testing"
)

func TestFake(t *testing.T) {
	out := func(f *Fake, args ...string) string {
		cmd := exec.Command(f.Path, args...)
		cmd.Stdin = strings.NewReader("the prompt")
		b, _ := cmd.Output()
		return strings.TrimSpace(string(b))
	}
	t.Run("replies are served in turn, the last repeats, and a new script replaces the old", func(t *testing.T) {
		f := New(t)
		f.Script(Reply{Lines: []string{"one"}}, Reply{Lines: []string{"two"}}, Reply{Lines: []string{"three"}})
		got := []string{out(f), ""}
		f.Script(Reply{Lines: []string{"nine"}})
		got[1] = out(f) + "," + out(f)
		if got[0] != "one" || got[1] != "nine,nine" {
			t.Errorf("replies = %q", got)
		}
	})
	t.Run("a call records its arguments, stdin, and the exit code is the scripted one", func(t *testing.T) {
		f := New(t)
		f.Script(Reply{ExitCode: 3, Stderr: "oops"})
		err := exec.Command(f.Path, "-p", "", "two words", "--flag").Run()
		calls := f.Calls()
		exit, _ := err.(*exec.ExitError)
		if exit == nil || exit.ExitCode() != 3 || len(calls) != 1 || strings.Join(calls[0].Args, "|") != "-p||two words|--flag" {
			t.Errorf("err = %v, calls = %+v", err, calls)
		}
		if v, ok := calls[0].Arg("-p"); !ok || v != "" {
			t.Errorf("Arg(-p) = %q, %v", v, ok)
		}
	})
	t.Run("a fake never scripted prints nothing and exits 0", func(t *testing.T) {
		f := New(t)
		if got := out(f); got != "" || len(f.Calls()) != 1 || f.Calls()[0].Stdin != "the prompt" {
			t.Errorf("out = %q, calls = %+v", got, f.Calls())
		}
	})
}
