package providertest

import (
	"os/exec"
	"reflect"
	"testing"
)

func TestFake(t *testing.T) {
	run := func(f *Fake, args ...string) (string, int) {
		out, err := exec.Command(f.Path, args...).Output()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
		return string(out), code
	}
	t.Run("each subcommand answers its replies in turn, the last repeating, and the calls are recorded", func(t *testing.T) {
		f := New(t, Script{Up: []Reply{{Exit: 4}, {Stdout: "A=1\n"}}, Smoke: []Reply{{Stdout: "smoke: FAIL (x)\n", Exit: 1}}})
		for _, want := range []struct {
			args []string
			out  string
			code int
		}{
			{[]string{"down", "--pr", "5"}, "", 0},
			{[]string{"up", "--pr", "5"}, "", 4},
			{[]string{"up", "--pr", "5"}, "A=1\n", 0},
			{[]string{"up", "--pr", "5"}, "A=1\n", 0},
			{[]string{"smoke"}, "smoke: FAIL (x)\n", 1},
		} {
			if out, code := run(f, want.args...); out != want.out || code != want.code {
				t.Errorf("%v = %q, %d; want %q, %d", want.args, out, code, want.out, want.code)
			}
		}
		if want := []string{"down --pr 5", "up --pr 5", "up --pr 5", "up --pr 5", "smoke"}; !reflect.DeepEqual(f.Calls(), want) {
			t.Errorf("Calls = %q", f.Calls())
		}
	})
	t.Run("every exit code of the contract can be scripted", func(t *testing.T) {
		for _, code := range []int{0, 1, 2, 3, 4, 7} {
			f := New(t, Script{Up: []Reply{{Exit: code}}})
			if _, got := run(f, "up", "--pr", "1"); got != code {
				t.Errorf("exit %d, want %d", got, code)
			}
		}
	})
}
