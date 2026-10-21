// Package docsgate is the docs-only gate: whether a research PR may be
// merged with no review and no Verification round. A research PR touches
// nothing but the research docs, so green CI and the gate's own reading of
// the real diff are the whole bar. The gate decides and never mutates: it
// is a pure function of what Go read, and Go merges on its verdict, pinned
// to the head it read.
package docsgate

import (
	"fmt"
	"strings"

	"github.com/benjr70/auto-agent/internal/ci"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/step/research"
)

// Reason is why the gate refused.
type Reason string

const (
	// NotDocsOnly: the diff is empty, or a changed path lives outside the
	// research prefix.
	NotDocsOnly Reason = "not-docs-only"
	// ChecksNotGreen: a check is failing or still pending.
	ChecksNotGreen Reason = Reason(ci.NotGreen)
	// ChecksMissing: no check ran at all, or a required check did not run
	// and pass.
	ChecksMissing Reason = Reason(ci.Missing)
	// ChecksUnreadable: the checks could not be read.
	ChecksUnreadable Reason = "checks-unreadable"
	// HeadMissing: the head the PR is at is not the one whose diff was read.
	// The gate could not run; it says nothing about the PR.
	HeadMissing Reason = "head-missing"
)

// Facts are what the gate decides on, as Go read them.
type Facts struct {
	// SHA is the PR's head as GitHub said it is: what a merge is pinned to.
	SHA string
	// Changed are the paths the diff of that head against the default
	// branch changes, with no rename detection; nil when the head was not
	// in the clone to diff.
	Changed []string
	// Prefix is the Harness config's docs_research_prefix.
	Prefix string
	// Checks are the checks on the head; ChecksErr why they could not be
	// read.
	Checks    []github.Check
	ChecksErr error
	// Required are the Harness config's required_checks.
	Required []string
}

// Verdict is what the gate decided.
type Verdict struct {
	// DocsOnly: the diff is non-empty and every path is under the prefix.
	DocsOnly bool `json:"docsOnly"`
	// SHA is the head the verdict is about, and the one a merge is pinned
	// to.
	SHA     string   `json:"sha"`
	Changed []string `json:"changed"`
	// Reason is why the gate refused, and Detail says more; both are empty
	// when it approved.
	Reason Reason `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Approved says the PR may be merged, at SHA.
func (v Verdict) Approved() bool { return v.Reason == "" }

// String is the verdict as a refusal reads in a comment and the Fire record.
func (v Verdict) String() string {
	if v.Approved() {
		return "approved at " + v.SHA
	}
	if v.Detail == "" {
		return string(v.Reason)
	}
	return string(v.Reason) + ": " + v.Detail
}

// Decide is the gate. It refuses a head it could not diff, then a diff that
// is not docs only, before it looks at a single check: a PR that is refused
// on its paths does not need its CI read. Every check must be green and
// every required one must have run and passed: the checks stand in for the
// review the PR skips.
func Decide(f Facts) Verdict {
	v := Verdict{SHA: f.SHA, Changed: f.Changed}
	if v.Changed == nil {
		v.Changed = []string{}
		v.Reason, v.Detail = HeadMissing, fmt.Sprintf("the head %s was not in the clone to diff", f.SHA)
		return v
	}
	if v.DocsOnly = research.DocsOnly(f.Changed, f.Prefix); !v.DocsOnly {
		v.Reason = NotDocsOnly
		if len(f.Changed) == 0 {
			v.Detail = "the diff is empty"
		} else {
			v.Detail = strings.Join(outside(f.Changed, f.Prefix), ", ")
		}
		return v
	}
	if f.ChecksErr != nil {
		v.Reason, v.Detail = ChecksUnreadable, f.ChecksErr.Error()
		return v
	}
	if reason, detail := ci.Vouch(f.Checks, f.Required); reason != "" {
		v.Reason, v.Detail = Reason(reason), detail
	}
	return v
}

// outside are the paths not under prefix.
func outside(paths []string, prefix string) []string {
	var out []string
	for _, path := range paths {
		if !strings.HasPrefix(path, prefix) {
			out = append(out, path)
		}
	}
	return out
}
