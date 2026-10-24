package depsgate

import (
	"errors"
	"testing"

	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/state"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// approvable are the facts of an open, non-draft Dependabot PR at sha
// whose markers vouch for it, with green checks and a deps title.
func approvable() Facts {
	return Facts{
		PR: github.PullRequest{
			Number: 636, Title: "chore(deps): bump axios from 1.1.0 to 1.1.2", State: "open",
			Author: state.DependabotLogin, HeadRef: "dependabot/npm_and_yarn/axios-1.1.2", HeadSHA: sha,
		},
		Gated:   sha,
		History: state.DepsHistory{SHA: sha, TierA: true, TierB: true},
		Checks:  []github.Check{{Name: "unit", Bucket: github.BucketPass}, {Name: "docs", Bucket: github.BucketSkipping}},
	}
}

// The cases of lib/deps-gate.test.sh, under the same names. Dropped, as
// they test what the Go gate does not have:
//   - "mergeCmd is harness_merge_recipe verbatim, shared with the docs
//     gate": there is no merge command to print; Go merges through the REST
//     API with the gated sha pinned (internal/cli/deps_test.go).
//   - "bad or missing args report reason usage" and "a trailing value-less
//     flag refuses with usage instead of hanging": the gate takes typed
//     Facts, not flags, so there is no usage to get wrong.
//   - "--security accepts any value and never changes the verdict": Facts
//     carry no security flag, so it cannot change the verdict.
//   - "the gate makes only read calls, whatever the verdict": Decide is a
//     pure function with no client to call; the Fire's reads and writes
//     around it are internal/cli/deps_test.go's.
func TestDecide(t *testing.T) {
	red := github.Check{Name: "unit", Bucket: github.BucketFail}
	pending := github.Check{Name: "unit", Bucket: github.BucketPending}
	cases := []struct {
		name   string
		change func(*Facts)
		reason Reason
	}{
		{"a green, marker-vouched Dependabot PR is approved with a pinned merge command", func(*Facts) {}, ""},
		{"a closed PR is refused", func(f *Facts) { f.PR.State = "closed" }, DraftOrClosed},
		{"a draft PR is refused", func(f *Facts) { f.PR.Draft = true }, DraftOrClosed},
		{"a human author is refused", func(f *Facts) { f.PR.Author = "some-human" }, NotDependabot},
		{"a non-dependabot branch is refused", func(f *Facts) { f.PR.HeadRef = "feat/axios" }, NotDependabot},
		// The REST API names the app dependabot[bot], where gh's listing
		// said app/dependabot: the lookalike is a human account's name.
		{"a lookalike login is refused", func(f *Facts) { f.PR.Author = "dependabot" }, NotDependabot},
		{"the app login is matched case-insensitively", func(f *Facts) { f.PR.Author = "Dependabot[bot]" }, ""},
		{"a missing tierA marker is refused", func(f *Facts) { f.History.TierA = false }, MarkersStale},
		{"a missing tierB marker is refused", func(f *Facts) { f.History.TierB = false }, MarkersStale},
		{"markers for another sha are refused", func(f *Facts) { f.History.SHA = "fedcba" }, MarkersStale},
		{"a PR that moved past --head is refused", func(f *Facts) { f.PR.HeadSHA = "fedcba" }, MarkersStale},
		{"a red check is refused despite gh exit 1", func(f *Facts) { f.Checks = []github.Check{red} }, ChecksNotGreen},
		{"a pending check is refused despite gh exit 8", func(f *Facts) { f.Checks = []github.Check{pending} }, ChecksPending},
		{"an empty check list is refused", func(f *Facts) { f.Checks = nil }, ChecksMissing},
		{"an unreadable check list is refused", func(f *Facts) { f.ChecksErr = errors.New("502") }, ChecksUnreadable},
		{"a chore(deps-dev) title is refused", func(f *Facts) { f.PR.Title = "chore(deps-dev): bump jest from 29.0.0 to 29.1.0" }, TitleNotDeps},
		{"a non-conventional title is refused", func(f *Facts) { f.PR.Title = "Bump axios from 1.1.0 to 1.1.2" }, TitleNotDeps},
		{"a fix(deps): title is accepted", func(f *Facts) { f.PR.Title = "fix(deps): bump axios from 1.1.0 to 1.1.2" }, ""},
		{"an absent required check is refused", func(f *Facts) { f.Required = []string{"pr-title"} }, ChecksMissing},
		{"a skipped required check is refused", func(f *Facts) { f.Required = []string{"docs"} }, ChecksMissing},
		{"a red required check is refused", func(f *Facts) {
			f.Required, f.Checks = []string{"pr-title"}, append(f.Checks, github.Check{Name: "pr-title", Bucket: github.BucketFail})
		}, ChecksNotGreen},
		{"a passed required check approves", func(f *Facts) { f.Required = []string{"unit"} }, ""},
		{"an empty required_checks demands no named check", func(f *Facts) { f.Required = []string{} }, ""},
		{"an unreviewed major bump is refused", func(f *Facts) { f.Major = true }, MajorUnapproved},
		{"a changes-requested major bump is refused", func(f *Facts) { f.Major, f.Reviewed.ChangesRequested = true, true }, ChangesRequested},
		{"an APPROVED major bump is approved", func(f *Facts) { f.Major, f.Reviewed.Approved = true, true }, ""},
		{"an unreviewed non-major bump is approved", func(f *Facts) { f.Major = false }, ""},
		{"a changes-requested non-major bump is refused", func(f *Facts) { f.Reviewed.ChangesRequested = true }, ChangesRequested},
		{"a changes-requested bump is refused by default", func(f *Facts) { f.Reviewed = state.Reviewed{ChangesRequested: true} }, ChangesRequested},
		{"a REVIEW_REQUIRED non-major bump is still approved", func(f *Facts) { f.Reviewed = state.Reviewed{} }, ""},
		{"an unreadable PR view is a harness error", func(f *Facts) { f.PRErr = errors.New("502") }, ChecksUnreadable},
		// Beyond the shell's cases.
		{"unreadable reviews are a harness error", func(f *Facts) { f.ReviewsErr = errors.New("502") }, ChecksUnreadable},
		{"a merged PR is refused", func(f *Facts) { f.PR.State, f.PR.Merged = "closed", true }, DraftOrClosed},
		{"a cancelled check is red, not pending", func(f *Facts) { f.Checks = []github.Check{{Name: "unit", Bucket: github.BucketCancel}} }, ChecksNotGreen},
		{"a red check beside a pending one waits for the run to settle", func(f *Facts) { f.Checks = []github.Check{red, {Name: "e2e", Bucket: github.BucketPending}} }, ChecksPending},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			facts := approvable()
			tt.change(&facts)
			v := Decide(facts)
			if v.Reason != tt.reason || v.SHA != sha || v.Approved() != (tt.reason == "") {
				t.Errorf("verdict %+v, want reason %q at %s", v, tt.reason, sha)
			}
			if !v.Approved() && v.Detail == "" {
				t.Errorf("a refusal says why: %+v", v)
			}
		})
	}
}

func TestVerdictHuman(t *testing.T) {
	for reason, human := range map[Reason]bool{
		TitleNotDeps: true, ChangesRequested: true, MajorUnapproved: true,
		ChecksNotGreen: false, ChecksPending: false, ChecksMissing: false, ChecksUnreadable: false, MarkersStale: false, DraftOrClosed: false, NotDependabot: false, "": false,
	} {
		if got := (Verdict{Reason: reason}).Human(); got != human {
			t.Errorf("%q: Human() = %v, want %v", reason, got, human)
		}
	}
}
