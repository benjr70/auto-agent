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
	t.Run("it keeps the environments it booted: up adds one, down --pr removes it, down --all removes them all", func(t *testing.T) {
		f := New(t, Script{Live: []int{9}})
		run(f, "up", "--pr", "5")
		run(f, "up", "--pr", "6")
		if want := []int{5, 6, 9}; !reflect.DeepEqual(f.Live(), want) {
			t.Fatalf("Live = %v, want %v", f.Live(), want)
		}
		run(f, "down", "--pr", "6")
		if want := []int{5, 9}; !reflect.DeepEqual(f.Live(), want) {
			t.Fatalf("Live = %v, want %v", f.Live(), want)
		}
		if _, code := run(f, "down", "--all"); code != 0 || len(f.Live()) != 0 {
			t.Errorf("down --all = %d, Live = %v", code, f.Live())
		}
	})
	t.Run("down --all answers its own replies, and a down that fails removes nothing", func(t *testing.T) {
		f := New(t, Script{Live: []int{3}, Down: []Reply{{Exit: 1}}, DownAll: []Reply{{Exit: 2, Stderr: "unknown option --all\n"}}})
		if _, code := run(f, "down", "--pr", "3"); code != 1 {
			t.Errorf("down --pr = %d, want 1", code)
		}
		if _, code := run(f, "down", "--all"); code != 2 {
			t.Errorf("down --all = %d, want 2", code)
		}
		if want := []int{3}; !reflect.DeepEqual(f.Live(), want) {
			t.Errorf("Live = %v, want %v", f.Live(), want)
		}
		if want := []string{"down --pr 3", "down --all"}; !reflect.DeepEqual(f.Calls(), want) {
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
