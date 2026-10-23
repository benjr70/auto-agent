package state

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github"
)

// pr is an open Agent PR by agent-bot, as the triage reads it.
func pr(number int, branch string, mergeable github.Mergeability, created string, labels ...string) OpenPR {
	return OpenPR{PullRequest: github.PullRequest{
		Number: number, HeadRef: branch, Mergeable: mergeable, CreatedAt: created, Labels: append([]string{}, labels...), Author: "agent-bot", State: "open",
	}}
}

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

// with sets what the triage read beyond the listing.
func (p OpenPR) with(review, verify, docs *bool) OpenPR {
	p.ReviewDone, p.VerifyDone, p.DocsOnly = review, verify, docs
	return p
}

func (p OpenPR) by(author string) OpenPR { p.Author = author; return p }
func (p OpenPR) draft() OpenPR           { p.Draft = true; return p }
func (p OpenPR) titled(t string) OpenPR  { p.Title = t; return p }
func (p OpenPR) replied(v *bool) OpenPR  { p.RulingOwed = v; return p }

// verdict is the triage's answer, as "pr reason issue", or "none".
func verdict(prs []OpenPR, login string) string {
	r := Triage(prs, login)
	if r == nil {
		return "none"
	}
	return strings.Join([]string{strconv.Itoa(r.PR), string(r.Reason), strconv.Itoa(r.Issue)}, " ")
}

// The cases carry over from lib/pr-triage.test.sh, under the same names, for
// the pick (pr_triage_pick) and the parked list (pr_triage_parked). The
// enrich and scan cases are the Fire's: internal/cli/reconcile_test.go.
func TestTriage(t *testing.T) {
	const mergeable, conflicting, unknown = github.Mergeable, github.Conflicting, github.MergeableUnknown
	cases := []struct {
		name  string
		prs   []OpenPR
		login string
		want  string
	}{
		{"conflicting team PR is picked", []OpenPR{pr(310, "feat/issue-281", conflicting, "2026-07-09T10:00:00Z")}, "", "310 conflict 281"},
		{"AFK:revise outranks CONFLICTING", []OpenPR{
			pr(310, "feat/issue-281", conflicting, "2026-07-01T10:00:00Z"),
			pr(311, "feat/issue-282", mergeable, "2026-07-09T10:00:00Z", LabelRevise),
		}, "", "311 revise 282"},
		{"oldest PR wins within the same reason", []OpenPR{
			pr(312, "feat/issue-283", mergeable, "2026-07-09T10:00:00Z", LabelRevise),
			pr(311, "feat/issue-282", mergeable, "2026-07-05T10:00:00Z", LabelRevise),
		}, "", "311 revise 282"},
		{"ours-filter excludes drafts / foreign branches / foreign authors", []OpenPR{
			pr(320, "feat/issue-290", conflicting, "2026-07-01T10:00:00Z").draft(),
			pr(321, "hotfix/human-branch", conflicting, "2026-07-01T10:00:00Z", LabelRevise),
			pr(322, "feat/issue-291", conflicting, "2026-07-01T10:00:00Z").by("some-human"),
		}, "agent-bot", "none"},
		{"the author filter ignores case, as GitHub logins do", []OpenPR{pr(322, "feat/issue-291", conflicting, "2026-07-01T10:00:00Z").by("Agent-Bot")}, "agent-bot", "322 conflict 291"},
		{"empty PR_TRIAGE_AUTHOR disables the author filter", []OpenPR{pr(324, "feat/issue-293", conflicting, "2026-07-01T10:00:00Z").by("whoever")}, "", "324 conflict 293"},
		{"mergeable/unknown/escalated PRs are not picked", []OpenPR{
			pr(330, "feat/issue-294", mergeable, "2026-07-01T10:00:00Z"),
			pr(331, "feat/issue-295", unknown, "2026-07-01T10:00:00Z"),
			pr(332, "feat/issue-296", conflicting, "2026-07-01T10:00:00Z", LabelRebaseFailed),
			pr(333, "feat/issue-297", mergeable, "2026-07-01T10:00:00Z", LabelRevise, LabelReviseFailed),
		}, "", "none"},
		{"empty list and malformed input degrade to no-pick", nil, "", "none"},
		{"bot-incomplete PR is picked", []OpenPR{pr(400, "feat/issue-350", mergeable, "2026-08-01T10:00:00Z").with(no(), yes(), nil)}, "", "400 incomplete 350"},
		{"bot-incomplete PR is picked: a verification round missing is enough", []OpenPR{pr(400, "feat/issue-350", mergeable, "2026-08-01T10:00:00Z").with(yes(), no(), nil)}, "", "400 incomplete 350"},
		{"complete PR is not picked", []OpenPR{pr(400, "feat/issue-350", mergeable, "2026-08-01T10:00:00Z").with(yes(), yes(), nil)}, "", "none"},
		{"an unread tail reads as complete: a broken sensor starts no work", []OpenPR{pr(400, "feat/issue-350", mergeable, "2026-08-01T10:00:00Z")}, "", "none"},
		{"revise > conflict > incomplete rank order", []OpenPR{
			pr(400, "feat/issue-350", mergeable, "2026-07-01T10:00:00Z").with(no(), no(), nil),
			pr(401, "feat/issue-351", conflicting, "2026-07-05T10:00:00Z"),
			pr(402, "feat/issue-352", mergeable, "2026-07-09T10:00:00Z", LabelRevise),
		}, "", "402 revise 352"},
		{"revise > conflict > incomplete rank order: conflict before incomplete", []OpenPR{
			pr(400, "feat/issue-350", mergeable, "2026-07-01T10:00:00Z").with(no(), no(), nil),
			pr(401, "feat/issue-351", conflicting, "2026-07-05T10:00:00Z"),
		}, "", "401 conflict 351"},
		{"revise > conflict > incomplete rank order: two incompletes, oldest first", []OpenPR{
			pr(405, "feat/issue-355", mergeable, "2026-07-08T10:00:00Z").with(no(), no(), nil),
			pr(404, "feat/issue-354", mergeable, "2026-07-02T10:00:00Z").with(no(), no(), nil),
		}, "", "404 incomplete 354"},
		{"docs-only PR picks reason docs-merge", []OpenPR{pr(590, "feat/issue-577", mergeable, "2026-08-28T10:00:00Z").with(no(), no(), yes())}, "", "590 docs-merge 577"},
		{"revise > conflict > docs-merge > incomplete: a docs PR that conflicts", []OpenPR{pr(590, "feat/issue-577", conflicting, "2026-08-28T10:00:00Z").with(nil, nil, yes())}, "", "590 conflict 577"},
		{"revise > conflict > docs-merge > incomplete: a docs PR handed back", []OpenPR{pr(590, "feat/issue-577", mergeable, "2026-08-28T10:00:00Z", LabelRevise).with(nil, nil, yes())}, "", "590 revise 577"},
		{"revise > conflict > docs-merge > incomplete: not docs, tail unfinished", []OpenPR{pr(591, "feat/issue-578", mergeable, "2026-08-28T10:00:00Z").with(no(), no(), no())}, "", "591 incomplete 578"},
		{"revise > conflict > docs-merge > incomplete: docs-merge before an older incomplete", []OpenPR{
			pr(591, "feat/issue-578", mergeable, "2026-08-20T10:00:00Z").with(no(), no(), no()),
			pr(590, "feat/issue-577", mergeable, "2026-08-28T10:00:00Z").with(no(), no(), yes()),
		}, "", "590 docs-merge 577"},
		{"enrich docsOnly probe fails safe", []OpenPR{pr(590, "feat/issue-577", mergeable, "2026-08-28T10:00:00Z").with(no(), no(), nil)}, "", "590 incomplete 577"},
		{"research/<slug> PR is ours-shaped and picks docs-merge", []OpenPR{pr(594, "research/577-docs-only-merge", mergeable, "2026-08-28T10:00:00Z").with(no(), no(), yes())}, "agent-bot", "594 docs-merge 577"},
		{"issue number falls back to the title, then to null", []OpenPR{pr(595, "research/docs-only-merge", conflicting, "2026-08-28T10:00:00Z").titled("docs(research): docs-only merge recipe (#577)")}, "", "595 conflict 577"},
		{"issue number falls back to the title, then to null: none", []OpenPR{pr(596, "research/docs-only-merge", conflicting, "2026-08-28T10:00:00Z")}, "", "596 conflict 0"},
		{"widened ours-filter still excludes hand-made branches", []OpenPR{
			pr(597, "researchers/pet-branch", conflicting, "2026-08-28T10:00:00Z"),
			pr(598, "docs/hand-written", conflicting, "2026-08-28T10:00:00Z"),
			pr(599, "research/with/slash", conflicting, "2026-08-28T10:00:00Z"),
			pr(600, "feat/issue-12-extra", conflicting, "2026-08-28T10:00:00Z"),
		}, "", "none"},

		// The Ruling cases: RulingOwed is what the Fire read from the PR's
		// comments (ruling.Read).
		{"a human reply of 1A 2B after the request: reason ruling", []OpenPR{pr(610, "feat/issue-560", mergeable, "2026-10-01T09:00:00Z", LabelRuling).replied(yes())}, "agent-bot", "610 ruling 560"},
		{"the same PR with no reply is not a candidate", []OpenPR{pr(610, "feat/issue-560", mergeable, "2026-10-01T09:00:00Z", LabelRuling).replied(no())}, "agent-bot", "none"},
		{"rulingReplied false is no candidate, whatever else the payload says", []OpenPR{pr(610, "feat/issue-560", mergeable, "2026-10-01T09:00:00Z", LabelRuling).replied(no())}, "agent-bot", "none"},
		{"an un-nudged free-text reply on a conflicting PR: reason conflict, not ruling", []OpenPR{pr(610, "feat/issue-560", conflicting, "2026-10-01T09:00:00Z", LabelRuling).replied(no())}, "agent-bot", "610 conflict 560"},
		{"a parked AFK:ruling PR whose reply is not a Ruling: the park holds", []OpenPR{pr(610, "feat/issue-560", mergeable, "2026-10-01T09:00:00Z", LabelRuling).draft().replied(no())}, "agent-bot", "none"},
		{"a newer ruled PR is picked ahead of an older conflicting one", []OpenPR{
			pr(600, "feat/issue-550", conflicting, "2026-09-01T09:00:00Z"),
			pr(610, "feat/issue-560", mergeable, "2026-10-01T09:00:00Z", LabelRuling).replied(yes()),
		}, "agent-bot", "610 ruling 560"},
		{"a ruled PR that also conflicts is reason ruling", []OpenPR{pr(612, "feat/issue-562", conflicting, "2026-10-01T09:00:00Z", LabelRuling).replied(yes())}, "agent-bot", "612 ruling 562"},
		{"AFK:revise on another PR still comes first", []OpenPR{
			pr(610, "feat/issue-560", mergeable, "2026-09-01T09:00:00Z", LabelRuling).replied(yes()),
			pr(611, "feat/issue-561", mergeable, "2026-10-01T09:00:00Z", LabelRevise),
		}, "agent-bot", "611 revise 561"},
		{"a ruled PR a human also handed back keeps reason revise", []OpenPR{pr(610, "feat/issue-560", mergeable, "2026-09-01T09:00:00Z", LabelRuling, LabelRevise).replied(yes())}, "agent-bot", "610 revise 560"},
		{"a waiting PR that conflicts is still rebased (reason conflict)", []OpenPR{pr(610, "feat/issue-560", conflicting, "2026-10-01T09:00:00Z", LabelRuling).replied(no())}, "agent-bot", "610 conflict 560"},
		{"a waiting PR handed back with AFK:revise is still revised", []OpenPR{pr(610, "feat/issue-560", mergeable, "2026-10-01T09:00:00Z", LabelRuling, LabelRevise).replied(no())}, "agent-bot", "610 revise 560"},
		{"a failed comments read is no candidate", []OpenPR{pr(610, "feat/issue-560", mergeable, "2026-10-01T09:00:00Z", LabelRuling)}, "agent-bot", "none"},
		{"an un-enriched AFK:ruling PR is no candidate (the label alone says nothing)", []OpenPR{pr(610, "feat/issue-560", mergeable, "2026-10-01T09:00:00Z", LabelRuling)}, "agent-bot", "none"},
		{"draft + AFK:checks-failed + AFK:ruling, replied 1A 2B: reason ruling", []OpenPR{pr(613, "feat/issue-563", mergeable, "2026-09-01T09:00:00Z", "AFK:checks-failed", LabelRuling).draft().replied(yes())}, "agent-bot", "613 ruling 563"},
		{"AFK:revise-failed + AFK:ruling, replied 1A 2B: reason ruling", []OpenPR{pr(613, "feat/issue-563", conflicting, "2026-09-01T09:00:00Z", LabelRuling, LabelReviseFailed).replied(yes())}, "agent-bot", "613 ruling 563"},
		{"AFK:rebase-failed + AFK:ruling, replied 1A 2B: reason ruling", []OpenPR{pr(613, "feat/issue-563", conflicting, "2026-09-01T09:00:00Z", LabelRuling, LabelRebaseFailed).replied(yes())}, "agent-bot", "613 ruling 563"},
		{"a parked PR is picked for its Ruling only (not revise, not conflict)", []OpenPR{pr(613, "feat/issue-563", conflicting, "2026-09-01T09:00:00Z", LabelRuling, LabelRevise, LabelRebaseFailed).replied(yes())}, "agent-bot", "613 ruling 563"},
		{"a parked AFK:ruling PR with no reply (or an unread one) stays parked", []OpenPR{
			pr(613, "feat/issue-563", conflicting, "2026-09-01T09:00:00Z", LabelRuling, LabelRevise).draft().replied(no()),
			pr(614, "feat/issue-564", conflicting, "2026-09-01T09:00:00Z", LabelRuling, LabelRevise, LabelReviseFailed),
		}, "agent-bot", "none"},
		{"a parked AFK:ruling PR with a free-text reply stays parked", []OpenPR{pr(613, "feat/issue-563", mergeable, "2026-09-01T09:00:00Z", LabelRuling).draft().replied(no())}, "agent-bot", "none"},
		{"a parked PR without AFK:ruling is no candidate", []OpenPR{pr(613, "feat/issue-563", mergeable, "2026-09-01T09:00:00Z", "AFK:checks-failed").draft().replied(yes())}, "agent-bot", "none"},
		{"a replied draft ranks as any Ruling, oldest first", []OpenPR{
			pr(610, "feat/issue-560", mergeable, "2026-10-01T09:00:00Z", LabelRuling).replied(yes()),
			pr(613, "feat/issue-563", mergeable, "2026-09-01T09:00:00Z", LabelRuling).draft().replied(yes()),
		}, "agent-bot", "613 ruling 563"},
		{"the pick skips exactly the PRs pr_triage_parked lists", []OpenPR{
			pr(620, "feat/issue-570", conflicting, "2026-09-01T09:00:00Z", LabelReviseFailed),
			pr(622, "feat/issue-572", conflicting, "2026-09-01T09:00:00Z", "AFK:checks-failed").draft(),
			pr(623, "feat/issue-573", conflicting, "2026-10-01T09:00:00Z", "AFK:checks-failed"),
		}, "agent-bot", "623 conflict 573"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verdict(c.prs, c.login); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("the verdict names the branch", func(t *testing.T) {
		got := Triage([]OpenPR{pr(594, "research/577-docs-only-merge", mergeable, "2026-08-28T10:00:00Z").with(nil, nil, yes())}, "")
		if got == nil || got.Branch != "research/577-docs-only-merge" {
			t.Errorf("got %+v", got)
		}
	})
}

// pr_triage_parked's cases: the one definition of a PR parked for a human.
func TestParkedPR(t *testing.T) {
	t.Run("a draft, AFK:revise-failed, AFK:rebase-failed, AFK:deps-failed; not a non-draft AFK:checks-failed, not a human's PR", func(t *testing.T) {
		const m = github.Mergeable
		var prs []github.PullRequest
		for _, p := range []OpenPR{
			pr(620, "feat/issue-570", m, "", LabelReviseFailed),
			pr(621, "feat/issue-571", m, "", LabelRebaseFailed),
			pr(622, "feat/issue-572", m, "", "AFK:checks-failed").draft(),
			pr(623, "feat/issue-573", m, "", "AFK:checks-failed"),
			pr(624, "feat/issue-574", m, "", LabelRevise),
			pr(625, "my-branch", m, "").draft(),
			pr(626, "feat/issue-576", m, "").by("some-human").draft(),
			pr(628, "feat/issue-578", m, "", LabelDepsFailed),
			pr(629, "feat/issue-579", m, "", LabelRuling).draft(),
		} {
			prs = append(prs, p.PullRequest)
		}
		parked := []int{}
		for _, p := range prs {
			if Ours(p, "agent-bot") && Parked(p) {
				parked = append(parked, p.Number)
			}
		}
		if !reflect.DeepEqual(parked, []int{620, 621, 622, 628, 629}) {
			t.Errorf("got %v", parked)
		}
	})
}

// The reconcile Lane in the order between Lanes: after a crash is cleared
// up, ahead of resuming paused work and of any pick.
func TestDeriveReconcile(t *testing.T) {
	revise := []OpenPR{pr(55, "feat/issue-20", github.Mergeable, "2026-10-01T00:00:00Z", LabelRevise)}
	queue := []github.QueueIssue{{Number: 21, Title: "Next", Labels: []string{LabelAFK}}}
	t.Run("an Agent PR with AFK:revise is taken ahead of an eligible ticket", func(t *testing.T) {
		got := Derive(Facts{Login: "agent-bot", Queue: queue, OpenPRs: revise})
		if got.Kind != KindReconcile || got.Pick != nil || got.Reconcile == nil || *got.Reconcile != (Reconcile{PR: 55, Branch: "feat/issue-20", Issue: 20, Reason: ReasonRevise}) {
			t.Fatalf("got %+v", got)
		}
		if got.Next != (Next{Action: ActionRevise, Issue: 20, PR: 55}) {
			t.Errorf("next = %+v", got.Next)
		}
	})
	t.Run("a Slice PR whose Ruling request is answered has the answer answered, even as a draft", func(t *testing.T) {
		ruled := []OpenPR{pr(55, "feat/issue-20", github.Mergeable, "2026-10-01T00:00:00Z", LabelRuling).draft().replied(yes())}
		if got := Derive(Facts{Login: "agent-bot", OpenPRs: ruled}); got.Next != (Next{Action: ActionRuling, Issue: 20, PR: 55}) {
			t.Errorf("next = %+v", got.Next)
		}
	})
	t.Run("AFK:revise on a research PR is not handled by this build yet", func(t *testing.T) {
		research := []OpenPR{pr(56, "research/3-findings", github.Mergeable, "2026-10-01T00:00:00Z", LabelRevise)}
		if got := Derive(Facts{Login: "agent-bot", OpenPRs: research}); got.Next.Action != ActionNone || !strings.Contains(got.Next.Reason, "revise is not handled by this build yet") {
			t.Errorf("next = %+v", got.Next)
		}
	})
	t.Run("a research PR that is docs only is landed through the docs-only gate; a Slice PR that is docs only is not", func(t *testing.T) {
		docs := []OpenPR{pr(56, "research/which-gate", github.Mergeable, "2026-10-01T00:00:00Z").titled("docs(research): the gate (#20)").with(no(), no(), yes())}
		if got := Derive(Facts{Login: "agent-bot", Queue: queue, OpenPRs: docs}); got.Kind != KindReconcile || got.Next != (Next{Action: ActionDocsMerge, Issue: 20, PR: 56}) {
			t.Errorf("got %+v", got)
		}
		slice := []OpenPR{pr(55, "feat/issue-20", github.Mergeable, "2026-10-01T00:00:00Z").with(no(), no(), yes())}
		if got := Derive(Facts{Login: "agent-bot", OpenPRs: slice}); got.Next.Action != ActionNone || !strings.Contains(got.Next.Reason, "docs-merge is not handled") {
			t.Errorf("next = %+v", got.Next)
		}
	})
	t.Run("a merged research PR's open ticket is finished after paused work, ahead of any pick, whatever its labels", func(t *testing.T) {
		failed := []github.QueueIssue{{Number: 20, Title: "Which gate?", Labels: []string{LabelAFK, LabelFailed, "wayfinder:research"}}, queue[0]}
		got := Derive(Facts{Login: "agent-bot", Queue: failed, Merged: map[int]int{20: 56, 12: 40}})
		if got.Kind != KindFinish || got.Next != (Next{Action: ActionFinish, Issue: 12, PR: 40}) || got.Pick != nil {
			t.Errorf("got %+v", got)
		}
		if got := Derive(Facts{Login: "agent-bot", Paused: []Paused{{Issue: 30, Pauses: 1}}, Merged: map[int]int{20: 56}}); got.Kind != KindResume {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("ahead of resuming paused work", func(t *testing.T) {
		if got := Derive(Facts{Login: "agent-bot", Paused: []Paused{{Issue: 30, Pauses: 1}}, OpenPRs: revise}); got.Kind != KindReconcile || got.Paused != nil {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("after a crash is cleared up, and never while Parked or another Fire runs", func(t *testing.T) {
		for _, facts := range []Facts{{Held: []int{30}}, {Parked: 9}, {LiveFire: true}, {LockUnreadable: true}} {
			facts.OpenPRs, facts.Login = revise, "agent-bot"
			if got := Derive(facts); got.Kind == KindReconcile || got.Reconcile != nil {
				t.Errorf("facts %+v: got %+v", facts, got)
			}
		}
	})
	t.Run("an open Agent PR with nothing to reconcile leaves the pick alone", func(t *testing.T) {
		quiet := []OpenPR{pr(55, "feat/issue-20", github.Mergeable, "2026-10-01T00:00:00Z")}
		if got := Derive(Facts{Login: "agent-bot", Queue: queue, OpenPRs: quiet}); got.Kind != KindPick || got.Pick.Issue != 21 {
			t.Errorf("got %+v", got)
		}
	})
}

func TestParentOf(t *testing.T) {
	for body, want := range map[string]int{
		"## Parent\n\nSpec: #102 (build group 4)\n\n## What to build\n\nSee #5": 102,
		"## Parent PRD\n\n#74\n":                       74,
		"## What to build\n\nPart of #90\n":            0,
		"## Parent\n\nnone\n\n## Blocked by\n\n- #130": 0,
		"## Parenthood\n\n#3":                          0,
	} {
		if got := ParentOf(body); got != want {
			t.Errorf("ParentOf(%q) = %d, want %d", body, got, want)
		}
	}
}

func TestIsResearch(t *testing.T) {
	for ref, want := range map[string]bool{"research/gh-deps": true, "research/12-x": true, "feat/issue-3": false, "research/": false, "my-research/x": false} {
		if got := IsResearch(ref); got != want {
			t.Errorf("IsResearch(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestTicketOf(t *testing.T) {
	for _, tt := range []struct {
		name, branch, title string
		want                int
	}{
		{"a Slice branch", "feat/issue-7", "feat: x (#9)", 7},
		{"a research PR's (#N) title suffix wins over digits its slug starts with", "research/2026-roadmap-which-database", "docs(research): which database (#21)", 21},
		{"a research branch that starts with its ticket, the title naming none", "research/19-findings", "feat(a): thing", 19},
		{"else the first #N in the title", "research/gh-deps", "docs: about #4 and #5", 4},
		{"none says", "research/gh-deps", "docs: about", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := TicketOf(github.PullRequest{HeadRef: tt.branch, Title: tt.title}); got != tt.want {
				t.Errorf("TicketOf(%q, %q) = %d, want %d", tt.branch, tt.title, got, tt.want)
			}
		})
	}
}

// botPR is an open Bot PR, read by the triage unless unread() says not.
func botPR(number int, branch string, mergeable github.Mergeability, created string, labels ...string) OpenPR {
	p := pr(number, branch, mergeable, created, labels...).by(DependabotLogin)
	p.HeadSHA = "sha" + strconv.Itoa(number)
	p.Bot = &BotSignals{Security: true}
	return p
}

func (p OpenPR) version() OpenPR  { p.Bot.Security = false; return p }
func (p OpenPR) unread() OpenPR   { p.Bot = nil; return p }
func (p OpenPR) approved() OpenPR { p.Bot.Approved = true; return p }
func (p OpenPR) agent() OpenPR    { p.Bot.AgentCommits = true; return p }
func (p OpenPR) green() OpenPR    { p.Bot.TierA = true; return p }

// The Bot PR cases of lib/pr-triage.test.sh, under the same names, for the
// pick. The enrich and scan cases are the Fire's: internal/cli/deps_test.go.
func TestTriageBotPRs(t *testing.T) {
	const mergeable, conflicting = github.Mergeable, github.Conflicting
	cases := []struct {
		name string
		prs  []OpenPR
		want string
	}{
		{"Dependabot PR picked with the full deps verdict", []OpenPR{botPR(700, "dependabot/npm_and_yarn/apps/backend/axios-1.1.2", mergeable, "2026-09-01T10:00:00Z")}, "700 dependabot 0"},
		{"Dependabot impostors and parked shapes are refused", []OpenPR{
			botPR(710, "dependabot/npm_and_yarn/axios-9.9.9", mergeable, "2026-09-01T10:00:00Z").by("some-human"),
			botPR(712, "dependabot/npm_and_yarn/axios-9.9.7", mergeable, "2026-09-01T10:00:00Z").draft(),
			botPR(713, "dependabot/npm_and_yarn/axios-9.9.6", mergeable, "2026-09-01T10:00:00Z", LabelHITL),
			botPR(714, "dependabot/npm_and_yarn/axios-9.9.5", mergeable, "2026-09-01T10:00:00Z", LabelDepsFailed),
		}, "none"},
		{"HITL bot PR picked only once APPROVED", []OpenPR{botPR(750, "dependabot/npm_and_yarn/axios-2.0.0", mergeable, "2026-09-01T10:00:00Z", LabelHITL).approved()}, "750 dependabot 0"},
		{"conflicting bot PR picks conflict with agentCommits", []OpenPR{botPR(770, "dependabot/npm_and_yarn/axios-1.1.2", conflicting, "2026-09-01T10:00:00Z")}, "770 conflict 0"},
		{"a broken Dependabot probe yields no dependabot pick", []OpenPR{
			botPR(780, "dependabot/npm_and_yarn/axios-1.1.2", mergeable, "2026-09-01T10:00:00Z").unread(),
			botPR(781, "dependabot/npm_and_yarn/lodash-4.1.2", conflicting, "2026-09-01T10:00:00Z").unread(),
		}, "none"},
		{"HITL gate holds on the conflict path too", []OpenPR{botPR(810, "dependabot/npm_and_yarn/axios-2.0.0", conflicting, "2026-09-01T10:00:00Z", LabelHITL)}, "none"},
		{"HITL gate holds on the conflict path too: once APPROVED it surfaces", []OpenPR{botPR(810, "dependabot/npm_and_yarn/axios-2.0.0", conflicting, "2026-09-01T10:00:00Z", LabelHITL).approved()}, "810 conflict 0"},
		{"a Bot PR with Tier A green on its head is not taken: what is left is not in this build", []OpenPR{botPR(820, "dependabot/npm_and_yarn/axios-1.1.2", mergeable, "2026-09-01T10:00:00Z").green()}, "none"},
		{"a Bot PR with Tier A green on its head that conflicts is still rebased", []OpenPR{botPR(821, "dependabot/npm_and_yarn/axios-1.1.2", conflicting, "2026-09-01T10:00:00Z").green()}, "821 conflict 0"},
		{"a Tier A green Bot PR does not hold back a younger one", []OpenPR{
			botPR(822, "dependabot/npm_and_yarn/a-1", mergeable, "2026-01-01T10:00:00Z").green(),
			botPR(823, "dependabot/npm_and_yarn/b-2", mergeable, "2026-08-01T10:00:00Z"),
		}, "823 dependabot 0"},
		{"a bot verdict never derives a ticket number", []OpenPR{botPR(790, "dependabot/npm_and_yarn/axios-1.1.2", mergeable, "2026-09-01T10:00:00Z").titled("chore(deps): bump axios (#123)")}, "790 dependabot 0"},
		{"every bot candidate is probed, so security still outranks age", []OpenPR{
			botPR(731, "dependabot/npm_and_yarn/a-1", mergeable, "2026-01-01T10:00:00Z").version(),
			botPR(732, "dependabot/npm_and_yarn/b-2", mergeable, "2026-08-01T10:00:00Z"),
		}, "732 dependabot 0"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := verdict(tt.prs, "agent-bot"); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("a bot verdict carries what the Lane needs, and no ticket", func(t *testing.T) {
		r := Triage([]OpenPR{botPR(770, "dependabot/npm_and_yarn/axios-1.1.2", conflicting, "2026-09-01T10:00:00Z").agent()}, "agent-bot")
		if r == nil || !r.Bot || r.Issue != 0 || r.Security == nil || !*r.Security || r.AgentCommits == nil || !*r.AgentCommits {
			t.Fatalf("verdict %+v", r)
		}
		if next := reconcileNext(*r, nil); next.Action != ActionDeps || next.PR != 770 || next.Issue != 0 {
			t.Errorf("next %+v", next)
		}
	})
	t.Run("dependabot ranks below every agent reason, security then oldest", func(t *testing.T) {
		old := "2026-01-01T10:00:00Z"
		agents := map[string]OpenPR{
			"revise":     pr(740, "feat/issue-700", mergeable, "2026-09-01T10:00:00Z", LabelRevise),
			"conflict":   pr(740, "feat/issue-700", conflicting, "2026-09-01T10:00:00Z"),
			"docs-merge": pr(740, "research/700-x", mergeable, "2026-09-01T10:00:00Z").with(nil, nil, yes()),
			"incomplete": pr(740, "feat/issue-700", mergeable, "2026-09-01T10:00:00Z").with(no(), yes(), nil),
		}
		for reason, agent := range agents {
			for _, bot := range []OpenPR{botPR(730, "dependabot/npm_and_yarn/axios-1.1.2", mergeable, old), botPR(735, "dependabot/npm_and_yarn/axios-1.1.2", conflicting, old)} {
				if got := Triage([]OpenPR{bot, agent}, "agent-bot"); got == nil || got.PR != 740 || string(got.Reason) != reason {
					t.Errorf("an agent %s PR against bot PR #%d: %+v", reason, bot.Number, got)
				}
			}
		}
		for want, prs := range map[string][]OpenPR{
			// A conflicting bot PR leads the bot block.
			"735 conflict 0": {botPR(736, "dependabot/npm_and_yarn/x-1", mergeable, old), botPR(735, "dependabot/npm_and_yarn/axios-1.1.2", conflicting, "2026-05-01T10:00:00Z")},
			"732 dependabot 0": {
				botPR(731, "dependabot/npm_and_yarn/a-1", mergeable, old).version(),
				botPR(732, "dependabot/npm_and_yarn/b-2", mergeable, "2026-08-01T10:00:00Z"),
			},
			"734 dependabot 0": {
				botPR(733, "dependabot/npm_and_yarn/a-1", mergeable, "2026-08-01T10:00:00Z"),
				botPR(734, "dependabot/npm_and_yarn/b-2", mergeable, "2026-02-01T10:00:00Z"),
			},
		} {
			if got := verdict(prs, "agent-bot"); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		}
	})
}
