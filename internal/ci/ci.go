// Package ci reads a pull request's checks the way the harness always has:
// what vouches for a PR, and when a wait on CI is over. It is pure: checks
// in, a verdict out.
package ci

import (
	"fmt"
	"slices"
	"strings"

	"github.com/benjr70/auto-agent/internal/github"
)

// Reason is why a PR's checks do not vouch for it.
type Reason string

const (
	// Missing: nothing ran, or a required check did not run and pass.
	Missing Reason = "checks-missing"
	// NotGreen: a check is failing or still pending.
	NotGreen Reason = "checks-not-green"
)

// Vouch says whether a PR's checks stand in for a human review: every check
// passed or was skipped, and every required check is there and passed. A
// skipped required check vouches for nothing: the workflow behind it may be
// path-filtered. An empty list vouches for nothing either. reason and detail
// say why not; both are empty when the checks vouch.
func Vouch(checks []github.Check, required []string) (reason Reason, detail string) {
	if len(checks) == 0 {
		return Missing, "no checks at all; nothing vouches for the PR"
	}
	notGreen := 0
	for _, c := range checks {
		if c.Bucket != github.BucketPass && c.Bucket != github.BucketSkipping {
			notGreen++
		}
	}
	if notGreen > 0 {
		return NotGreen, fmt.Sprintf("%d check(s) are failing or pending", notGreen)
	}
	for _, name := range required {
		if !slices.ContainsFunc(checks, func(c github.Check) bool { return c.Name == name && c.Bucket == github.BucketPass }) {
			return Missing, "a required check did not run and pass: " + name
		}
	}
	return "", ""
}

// Result is where a wait on CI stands after one read of the checks.
type Result string

const (
	// Green: everything settled and nothing failed.
	Green Result = "green"
	// Red: everything settled and a check failed.
	Red Result = "fail"
	// Pending: a check is still running, or a required one has not run.
	Pending Result = "pending"
	// None: no check is on the commit at all.
	None Result = "none"
)

// Settle reads one poll of a wait on CI. A failed check is red only once
// nothing else is running, so its verdict covers the whole run; a cancelled
// one counts as failed. A skipped check is green. A required check that has
// not run and passed keeps the wait going, as a check that has not started
// yet would.
func Settle(checks []github.Check, required []string) Result {
	if len(checks) == 0 {
		return None
	}
	pending, failed := 0, 0
	for _, c := range checks {
		switch c.Bucket {
		case github.BucketPending:
			pending++
		case github.BucketFail, github.BucketCancel:
			failed++
		}
	}
	switch {
	case pending > 0:
		return Pending
	case failed > 0:
		return Red
	}
	for _, name := range required {
		if !slices.ContainsFunc(checks, func(c github.Check) bool { return c.Name == name && c.Bucket == github.BucketPass }) {
			return Pending
		}
	}
	return Green
}

// Failed are the checks that failed.
func Failed(checks []github.Check) []github.Check {
	var failed []github.Check
	for _, c := range checks {
		if c.Bucket == github.BucketFail || c.Bucket == github.BucketCancel {
			failed = append(failed, c)
		}
	}
	return failed
}

// Tail is the last n lines of a log.
func Tail(log string, n int) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
