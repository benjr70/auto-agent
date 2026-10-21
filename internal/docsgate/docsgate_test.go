package docsgate

import (
	"errors"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github"
)

// Ported from lib/docs-only-gate.test.sh, under its case names. The gate is
// a function now, so the cases about its command line are dropped:
//   - test_base_defaults_to_default_branch: the base is the Workspace's, the
//     default branch the clone was prepared from; the diff of the head is
//     internal/workspace's TestChangedAt.
//   - test_merge_cmd_is_the_shared_recipe: there is no command to share. Go
//     merges through the API, pinned to Verdict.SHA (internal/cli's research
//     Fire tests: a moved head is not merged).
//   - test_gate_never_mutates: Decide reads nothing and has no client to
//     write with.
//   - test_missing_args_usage_error: there are no arguments to miss.

func pass(name string) github.Check { return github.Check{Name: name, Bucket: github.BucketPass} }

func TestDecide(t *testing.T) {
	const prefix = "docs/research/"
	green := []github.Check{pass("test")}
	for _, tt := range []struct {
		name     string
		facts    Facts
		reason   Reason
		docsOnly bool
	}{
		{"test_docs_only_verdict", Facts{Changed: []string{"docs/research/577-merge-recipe.md", "docs/research/583-spec.md"}, Checks: green}, "", true},
		{"test_non_docs_path_refused", Facts{Changed: []string{"docs/research/577.md", "apps/backend/src/app.service.ts"}, Checks: green}, NotDocsOnly, false},
		{"test_prefix_comes_from_config: notes/ is outside the default prefix", Facts{Changed: []string{"notes/one.md", "notes/two.md"}, Checks: green}, NotDocsOnly, false},
		{"test_prefix_comes_from_config: with the prefix notes/ the same diff is docs only", Facts{Prefix: "notes/", Changed: []string{"notes/one.md", "notes/two.md"}, Checks: green}, "", true},
		// The diff has no rename detection, so a file moved into the prefix
		// still shows the path it left.
		{"test_rename_into_docs_research_refused", Facts{Changed: []string{"docs/research/moved-spec.md", "scripts/ralph/old-spec.md"}, Checks: green}, NotDocsOnly, false},
		{"test_empty_diff_refused", Facts{Changed: []string{}, Checks: green}, NotDocsOnly, false},
		{"test_approved_when_checks_green", Facts{Changed: []string{"docs/research/577-merge-recipe.md"}, Checks: green}, "", true},
		{"test_refused_when_checks_red", Facts{Changed: []string{"docs/research/577-merge-recipe.md"}, Checks: []github.Check{pass("test"), {Name: "lint", Bucket: github.BucketFail}}}, ChecksNotGreen, true},
		{"test_refused_when_checks_pending", Facts{Changed: []string{"docs/research/577-merge-recipe.md"}, Checks: []github.Check{{Name: "test", Bucket: github.BucketPending}}}, ChecksNotGreen, true},
		{"test_refused_when_checks_unreadable", Facts{Changed: []string{"docs/research/577-merge-recipe.md"}, ChecksErr: errors.New("github: 502")}, ChecksUnreadable, true},
		{"test_refused_when_checks_empty", Facts{Changed: []string{"docs/research/577-merge-recipe.md"}, Checks: []github.Check{}}, ChecksMissing, true},
		{"test_required_checks_from_config: an absent required check", Facts{Changed: []string{"docs/research/a.md"}, Required: []string{"Conventional PR title"}, Checks: green}, ChecksMissing, true},
		{"test_required_checks_from_config: a skipped required check", Facts{Changed: []string{"docs/research/a.md"}, Required: []string{"Conventional PR title"},
			Checks: []github.Check{{Name: "Conventional PR title", Bucket: github.BucketSkipping}, pass("test")}}, ChecksMissing, true},
		{"test_required_checks_from_config: a passed required check", Facts{Changed: []string{"docs/research/a.md"}, Required: []string{"Conventional PR title"},
			Checks: []github.Check{pass("Conventional PR title"), pass("test")}}, "", true},
		{"test_required_checks_from_config: an empty list demands no named check", Facts{Changed: []string{"docs/research/a.md"}, Checks: green}, "", true},
		{"test_missing_head_object_is_gate_error", Facts{Changed: nil, Checks: green}, HeadMissing, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.facts.SHA = "abc123"
			if tt.facts.Prefix == "" {
				tt.facts.Prefix = prefix
			}
			v := Decide(tt.facts)
			if v.Reason != tt.reason || v.DocsOnly != tt.docsOnly || v.SHA != "abc123" || v.Approved() != (tt.reason == "") {
				t.Errorf("verdict = %+v, want reason %q, docsOnly %v", v, tt.reason, tt.docsOnly)
			}
			if tt.reason != "" && v.Detail == "" {
				t.Errorf("a refusal says no more than %q", v.Reason)
			}
			if v.Changed == nil {
				t.Error("changed is null, not a list")
			}
		})
	}
	t.Run("a refusal on the paths names the paths outside the prefix, and costs no check read", func(t *testing.T) {
		v := Decide(Facts{SHA: "abc123", Prefix: prefix, Changed: []string{"docs/research/a.md", "lib/x.go", "README.md"}, ChecksErr: errors.New("never read")})
		if v.Reason != NotDocsOnly || v.String() != "not-docs-only: lib/x.go, README.md" {
			t.Errorf("verdict = %+v (%s)", v, v)
		}
	})
	t.Run("a refusal reads as its reason and why", func(t *testing.T) {
		v := Decide(Facts{SHA: "abc123", Prefix: prefix, Changed: []string{"docs/research/a.md"}, Checks: []github.Check{}})
		if !strings.HasPrefix(v.String(), "checks-missing: no checks at all") {
			t.Errorf("verdict reads %q", v)
		}
	})
}
