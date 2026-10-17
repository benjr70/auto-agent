package ci

import (
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github"
)

func check(name string, bucket github.Bucket) github.Check {
	return github.Check{Name: name, Bucket: bucket}
}

var green = []github.Check{check("test", github.BucketPass), check("lint", github.BucketSkipping)}

// Ported from lib/pr-checks.test.sh.
func TestVouch(t *testing.T) {
	for _, c := range []struct {
		name     string
		required []string
		checks   []github.Check
		want     Reason
	}{
		{"all green (pass and skipping) with nothing required vouches", nil, green, ""},
		{"an empty list is checks-missing", nil, nil, Missing},
		{"a failing check is checks-not-green", nil, []github.Check{check("test", github.BucketFail)}, NotGreen},
		{"a pending check is checks-not-green", nil, []github.Check{check("test", github.BucketPending)}, NotGreen},
		{"a cancelled check is checks-not-green", nil, []github.Check{check("test", github.BucketCancel)}, NotGreen},
		{"a required check that passed vouches", []string{"test"}, green, ""},
		{"a required check that is absent is checks-missing", []string{"title"}, green, Missing},
		{"a required check that was skipped is checks-missing", []string{"lint"}, green, Missing},
		{"not-green is reported before a missing required check", []string{"title"}, []github.Check{check("test", github.BucketFail)}, NotGreen},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := Vouch(c.checks, c.required); got != c.want {
				t.Errorf("Vouch = %q, want %q", got, c.want)
			}
		})
	}
	t.Run("the detail names the missing required check", func(t *testing.T) {
		if _, detail := Vouch(green, []string{"title"}); detail != "a required check did not run and pass: title" {
			t.Errorf("detail %q", detail)
		}
	})
}

// Ported from lib/ci-wait.test.sh: one poll's reading. The waiting itself
// is the Fire's, tested through it.
func TestSettle(t *testing.T) {
	for _, c := range []struct {
		name     string
		required []string
		checks   []github.Check
		want     Result
	}{
		{"a running check is pending", nil, []github.Check{check("build", github.BucketPending)}, Pending},
		{"a passed check is green", nil, []github.Check{check("build", github.BucketPass)}, Green},
		{"red -> exit 1, failed[] + '=== job ===' log tail", nil, []github.Check{check("unit-tests", github.BucketFail), check("lint", github.BucketPass)}, Fail},
		{"fail+pending waits for settle before verdict", nil, []github.Check{check("unit", github.BucketFail), check("e2e", github.BucketPending)}, Pending},
		{"skipping bucket ignored -> green", nil, []github.Check{check("docs", github.BucketSkipping), check("build", github.BucketPass)}, Green},
		{"a cancelled run is red: it said nothing green", nil, []github.Check{check("build", github.BucketCancel)}, Fail},
		{"no check on the commit is none, not green", nil, nil, None},
		{"no check on the commit while a check is required keeps the wait going: it has not reported yet", []string{"test"}, nil, Pending},
		{"a required check that has not run keeps the wait going", []string{"title"}, green, Pending},
		{"a required check that passed lets it end green", []string{"test"}, green, Green},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Settle(c.checks, c.required); got != c.want {
				t.Errorf("Settle = %q, want %q", got, c.want)
			}
		})
	}
	t.Run("the failed checks are the failing and the cancelled ones", func(t *testing.T) {
		got := Failed([]github.Check{check("a", github.BucketFail), check("b", github.BucketPass), check("c", github.BucketCancel)})
		if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
			t.Errorf("Failed = %+v", got)
		}
	})
}

func TestTail(t *testing.T) {
	log := strings.Repeat("line\n", 300) + "the error\n"
	if got := Tail(log, 200); strings.Count(got, "\n") != 199 || !strings.HasSuffix(got, "the error") {
		t.Errorf("tail has %d lines", strings.Count(got, "\n")+1)
	}
	if got := Tail("one\ntwo\n", 200); got != "one\ntwo" {
		t.Errorf("tail %q", got)
	}
}
