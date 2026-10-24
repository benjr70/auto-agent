package state

import (
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
)

// mine is a comment the Daemon, agent-bot, wrote.
func mine(bodies ...string) []github.Comment {
	comments := make([]github.Comment, len(bodies))
	for i, body := range bodies {
		comments[i] = github.Comment{ID: int64(i + 1), Author: "agent-bot", Body: body}
	}
	return comments
}

func history(t *testing.T, comments []github.Comment, sha string) DepsHistory {
	t.Helper()
	h, err := ReadDepsHistory(comments, "agent-bot", sha)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The cases carry over from lib/deps-lane.test.sh, under the same names,
// for the markers, the retitle, the checklist injection and the commit
// trailer. The lane gate is DepsLaneOff, here; the park and the cap from the
// config are the Fire's: internal/cli/deps_test.go.
func TestDepsMarkers(t *testing.T) {
	const sha = "deadbeefcafe1234567890abcdef0123456789ab"
	t.Run("marker emit prints one hidden comment per state", func(t *testing.T) {
		for got, want := range map[string]string{
			tierAMarker(sha):         "<!-- deps-lane tierA=green sha=" + sha + " -->",
			tierBMarker(sha):         "<!-- deps-lane tierB=PASS sha=" + sha + " -->",
			FixAttemptMarker(2, sha): "<!-- deps-lane fix-attempt=2 sha=" + sha + " -->",
			ParkMarker(sha):          "<!-- deps-lane parked sha=" + sha + " -->",
		} {
			if got != want || strings.Contains(got, "\n") {
				t.Errorf("marker %q, want %q", got, want)
			}
		}
		comment, err := TierAComment(sha)
		if err != nil || !strings.HasSuffix(comment, tierAMarker(sha)) {
			t.Errorf("Tier A comment %q, %v", comment, err)
		}
	})
	t.Run("parse round-trips each emitted marker state", func(t *testing.T) {
		if h := history(t, mine(tierAMarker(sha)+"\n"+tierBMarker(sha)), sha); h.SHA != sha || !h.TierA || !h.TierB {
			t.Errorf("history %+v", h)
		}
		// A marker inside a comment's prose is the same marker.
		if h := history(t, mine("Tier A is green for this bump.\n\n"+tierAMarker(sha)+"\n\nRe-run it."), sha); !h.TierA {
			t.Errorf("history %+v", h)
		}
	})
	t.Run("parse with no markers reports nothing green", func(t *testing.T) {
		if h := history(t, mine("Just a human comment, no markers here."), sha); h.TierA || h.TierB || h.Attempts != 0 || h.RoundsLeft(3) != 3 {
			t.Errorf("history %+v", h)
		}
	})
	t.Run("parse ignores markers from another sha", func(t *testing.T) {
		const head, old = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
		h := history(t, mine(tierAMarker(old), tierBMarker(old), FixAttemptMarker(1, old), FixAttemptMarker(2, old), tierAMarker(head)), head)
		if !h.TierA || h.TierB || h.Attempts != 0 {
			t.Errorf("history %+v", h)
		}
	})
	t.Run("parse counts fix attempts and reports the cap", func(t *testing.T) {
		const head = "1111111111111111111111111111111111111111"
		two := []string{FixAttemptMarker(1, head), FixAttemptMarker(2, head)}
		if h := history(t, mine(two...), head); h.Attempts != 2 || h.RoundsLeft(3) != 1 {
			t.Errorf("history %+v", h)
		}
		if h := history(t, mine(append(two, FixAttemptMarker(3, head))...), head); h.Attempts != 3 || h.RoundsLeft(3) != 0 {
			t.Errorf("history %+v", h)
		}
		// One comment re-stamping the whole history counts as many as it
		// carries; the number in a marker is never read back.
		stamped, _ := FixAttemptsComment(1, 2, 3, head)
		if h := history(t, mine(stamped, FixAttemptMarker(99, head)), head); h.Attempts != 3 {
			t.Errorf("history %+v", h)
		}
	})
	t.Run("parse matches the sha exactly, not by prefix", func(t *testing.T) {
		const short, long = "abc123", "abc1234567"
		for _, c := range [][2]string{{long, short}, {short, long}} {
			written, asked := c[0], c[1]
			if h := history(t, mine(tierAMarker(written), tierBMarker(written), FixAttemptMarker(1, written)), asked); h.TierA || h.TierB || h.Attempts != 0 {
				t.Errorf("markers for %s read for %s: %+v", written, asked, h)
			}
		}
	})
	t.Run("parse refuses an empty sha", func(t *testing.T) {
		if _, err := ReadDepsHistory(mine(tierAMarker("")), "agent-bot", ""); err == nil {
			t.Error("an empty sha read a history")
		}
		if _, err := TierAComment(""); err == nil {
			t.Error("an empty sha was given a marker")
		}
		if _, err := FixAttemptsComment(1, 1, 3, ""); err == nil {
			t.Error("an empty sha was given a fix-attempt marker")
		}
	})
	t.Run("only the Daemon's own markers count", func(t *testing.T) {
		comments := []github.Comment{{Author: "a-human", Body: tierAMarker(sha) + FixAttemptMarker(1, sha)}}
		if h := history(t, comments, sha); h.TierA || h.Attempts != 0 {
			t.Errorf("history %+v", h)
		}
	})
	t.Run("rounds-left is the cap minus the recorded attempts, floored at 0", func(t *testing.T) {
		for recorded, want := range map[int]int{0: 3, 1: 2, 2: 1, 3: 0, 4: 0, 9: 0} {
			if got := (DepsHistory{Attempts: recorded}).RoundsLeft(3); got != want {
				t.Errorf("RoundsLeft after %d is %d, want %d", recorded, got, want)
			}
		}
	})
}

func TestRetitle(t *testing.T) {
	t.Run("a security chore(deps) title is retitled fix(deps)", func(t *testing.T) {
		if got := Retitle("chore(deps): bump axios from 1.6.0 to 1.6.8", true); got != "fix(deps): bump axios from 1.6.0 to 1.6.8" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("non-security and foreign-prefix titles pass through unchanged", func(t *testing.T) {
		for title, security := range map[string]bool{
			"chore(deps): bump axios from 1.6.0 to 1.6.8":      false,
			"fix(deps): bump axios from 1.6.0 to 1.6.8":        true,
			"chore(deps-dev): bump jest from 29.0.0 to 29.7.0": true,
			"build: bump the docker base image":                true,
		} {
			if got := Retitle(title, security); got != title {
				t.Errorf("%q became %q", title, got)
			}
		}
	})
	t.Run("retitle is idempotent, group-safe and stdin-capable", func(t *testing.T) {
		once := Retitle("chore(deps): bump axios from 1.6.0 to 1.6.8", true)
		if twice := Retitle(once, true); twice != once {
			t.Errorf("once %q, twice %q", once, twice)
		}
		if got := Retitle("chore(deps): bump the npm-production group across 4 directories with 7 updates", true); got != "fix(deps): bump the npm-production group across 4 directories with 7 updates" {
			t.Errorf("got %q", got)
		}
	})
}

// markedChecklist is a bot-pr-checklist.md that carries its own markers,
// with notes after the end marker that never reach a PR.
const markedChecklist = `<!-- bot-pr-checklist v1 -->

## Manual verification

Tier B could not vouch for this PR on its own; confirm the items below by hand.

- [ ] Stack healthy: backend /health + /ready, device /health, frontend all 200
- [ ] Electron launches at 800x480, renders the Smoke screen, no console errors

<!-- /bot-pr-checklist -->

Maintainer notes: keep the items above in sync with the smoke suite. These
notes are for humans editing this file and must never be injected into a PR.
`

// proseChecklist is one as a maintainer writes it: no markers.
const proseChecklist = `## Manual verification

Run against the hermetic environment only.

- [ ] The test suite still passes after the bump
- [ ] /api/health answers on a fresh up
`

func inject(t *testing.T, body, file string) string {
	t.Helper()
	out, err := InjectChecklist(body, file)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInjectChecklist(t *testing.T) {
	t.Run("inject appends the checklist unit to a body lacking the marker", func(t *testing.T) {
		out := inject(t, "", markedChecklist)
		if !strings.HasPrefix(out, ChecklistMarker+"\n") || !strings.HasSuffix(out, ChecklistEnd+"\n") || strings.Contains(out, "Maintainer notes") {
			t.Errorf("into an empty body:\n%s", out)
		}
		body := "Bumps [axios](https://github.com/axios/axios) from 1.6.0 to 1.6.8.\n\n## Release notes\n\nSourced from axios releases.\n"
		out = inject(t, body, markedChecklist)
		if !strings.HasPrefix(out, body+"\n"+ChecklistMarker) || strings.Count(out, ChecklistMarker) != 1 || strings.Contains(out, "Maintainer notes") || !strings.Contains(out, "- [ ] Stack healthy") {
			t.Errorf("into Dependabot's body:\n%s", out)
		}
	})
	t.Run("inject is a byte-level no-op on a body carrying the marker", func(t *testing.T) {
		// Ticked by a maintainer, and no trailing newline, as GitHub sends it.
		body := "Bumps axios from 1.6.0 to 1.6.8.\n\n" + ChecklistMarker + "\n\n## Manual verification\n\n- [x] Stack healthy\n\n" + ChecklistEnd
		if out := inject(t, body, markedChecklist); out != body {
			t.Errorf("changed:\n%q", out)
		}
		first := inject(t, "Bumps axios from 1.6.0 to 1.6.8.\n", markedChecklist)
		if second := inject(t, first, markedChecklist); second != first || strings.Count(second, ChecklistMarker) != 1 {
			t.Errorf("first %q, second %q", first, second)
		}
	})
	t.Run("inject wraps a marker-less checklist verbatim in the harness markers", func(t *testing.T) {
		out := inject(t, "Bumps axios from 1.6.0 to 1.6.8.\n", proseChecklist)
		want := "Bumps axios from 1.6.0 to 1.6.8.\n\n" + ChecklistMarker + "\n\n" + proseChecklist + "\n" + ChecklistEnd + "\n"
		if out != want {
			t.Errorf("got\n%q\nwant\n%q", out, want)
		}
		if again := inject(t, out, proseChecklist); again != out {
			t.Errorf("injected twice:\n%q", again)
		}
	})
	t.Run("inject refuses a checklist Tier B could not act on", func(t *testing.T) {
		for _, file := range []string{"# Bot PR checklist\n\n- The tests pass.\n", "", "## Notes\n\n- [ ] not under a verification heading\n"} {
			if out, err := InjectChecklist("Body.\n", file); err == nil || out != "" || !strings.Contains(err.Error(), "Manual verification") {
				t.Errorf("%q: %q, %v", file, out, err)
			}
		}
	})
}

func TestCommitTrailer(t *testing.T) {
	trailer := func(t *testing.T, message string) string {
		t.Helper()
		out, err := CommitTrailer(message)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Run("commit-trailer appends [dependabot skip] to the end of the message", func(t *testing.T) {
		if got := trailer(t, "fix(ci): pr-watch round 1 — auto-fix failing checks\n"); got != "fix(ci): pr-watch round 1 — auto-fix failing checks [dependabot skip]\n" {
			t.Errorf("got %q", got)
		}
		got := trailer(t, "fix(ci): pr-watch round 2\n\nRegenerated package-lock.json with\n--legacy-peer-deps.\n")
		if got != "fix(ci): pr-watch round 2\n\nRegenerated package-lock.json with\n--legacy-peer-deps. [dependabot skip]\n" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("commit-trailer is idempotent, newline-sane and refuses empty", func(t *testing.T) {
		once := trailer(t, "fix(ci): round 1\n")
		if twice := trailer(t, once); twice != once {
			t.Errorf("once %q, twice %q", once, twice)
		}
		for in, want := range map[string]string{
			"fix(ci): round 1\n\n\n":                         "fix(ci): round 1 [dependabot skip]\n",
			"fix(ci): round 1":                               "fix(ci): round 1 [dependabot skip]\n",
			"fix(ci): round 1 [dependabot skip] \n":          "fix(ci): round 1 [dependabot skip]\n",
			"fix(ci): keep [dependabot skip] in the subject": "fix(ci): keep [dependabot skip] in the subject\n",
			"fix(ci): [dependabot skip] [dependabot skip]":   "fix(ci): [dependabot skip] [dependabot skip]\n",
		} {
			if got := trailer(t, in); got != want {
				t.Errorf("%q became %q, want %q", in, got, want)
			}
		}
		for _, empty := range []string{"", " \n\t\n"} {
			if out, err := CommitTrailer(empty); err == nil || out != "" {
				t.Errorf("%q: %q, %v", empty, out, err)
			}
		}
	})
}

func TestDepsLaneOff(t *testing.T) {
	t.Run("the lane is on exactly when the dependabot block is declared and enabled", func(t *testing.T) {
		for lane, want := range map[config.Lane]string{
			{Present: true, Enabled: true}:   "",
			{Present: true, Enabled: false}:  "dependabot.enabled is false",
			{Present: false, Enabled: false}: "the Harness config declares no dependabot block",
		} {
			if got := DepsLaneOff(lane); got != want {
				t.Errorf("%+v: %q, want %q", lane, got, want)
			}
		}
	})
}

func TestBotSignals(t *testing.T) {
	t.Run("depsSecurity from the footer, defaulting on dependabot.yml", func(t *testing.T) {
		footer := "Bumps axios.\n\nThis is an automated security fix for a known vulnerability."
		for _, c := range []struct {
			body       string
			configured bool
			want       bool
		}{
			{footer, true, true},
			{"Bumps axios. Routine update.", true, false},
			{"Bumps axios. Routine update.", false, true},
		} {
			if got := IsSecurity(c.body, c.configured); got != c.want {
				t.Errorf("%q with dependabot.yml %v: %v", c.body, c.configured, got)
			}
		}
	})
	t.Run("conflicting bot PR picks conflict with agentCommits", func(t *testing.T) {
		bot := github.Commit{Login: "dependabot[bot]", Name: "dependabot[bot]", Email: "49699333+dependabot[bot]@users.noreply.github.com"}
		if AgentCommits([]github.Commit{bot}) {
			t.Error("Dependabot's own commit counted as the Daemon's")
		}
		if !AgentCommits([]github.Commit{bot, {Login: "agent-bot", Name: "agent-bot", Email: "agent@example.com"}}) {
			t.Error("the Daemon's commit was not counted")
		}
	})
	t.Run("only an author-and-branch match is a Bot PR", func(t *testing.T) {
		for _, c := range []struct {
			author, branch string
			want           bool
		}{
			{"dependabot[bot]", "dependabot/npm_and_yarn/axios-1.1.2", true},
			{"Dependabot[bot]", "dependabot/npm_and_yarn/axios-1.1.2", true},
			{"some-human", "dependabot/npm_and_yarn/axios-9.9.9", false},
			{"dependabot", "dependabot/npm_and_yarn/axios-9.9.9", false},
			{"dependabot[bot]", "feat/issue-12", false},
		} {
			if got := IsBotPR(github.PullRequest{Author: c.author, HeadRef: c.branch}); got != c.want {
				t.Errorf("%s on %s: %v", c.author, c.branch, got)
			}
		}
	})
}

// The Tier B, hand-off and refusal markers.
func TestDepsTierBMarkers(t *testing.T) {
	const sha = "deadbeefcafe1234567890abcdef0123456789ab"
	t.Run("a Tier B comment carries its marker and the round's line", func(t *testing.T) {
		comment, err := TierBComment(sha, "manual-verify: 1/1 PASS, 0 deferred, 0 FAIL")
		if err != nil || !strings.Contains(comment, tierBMarker(sha)) || !strings.Contains(comment, "1/1 PASS") {
			t.Errorf("comment %q, err %v", comment, err)
		}
		if h := history(t, mine(comment), sha); !h.TierB || h.TierA {
			t.Errorf("history %+v", h)
		}
		if _, err := TierBComment("", "x"); err == nil {
			t.Error("a Tier B marker for no head was written")
		}
	})
	t.Run("a refusal is read for its head only, and only from the Daemon", func(t *testing.T) {
		refused := RefusedComment("title-not-deps", "the title is chore(deps-dev):", sha)
		if h := history(t, mine(refused), sha); h.Refused != "title-not-deps" {
			t.Errorf("history %+v", h)
		}
		if h := history(t, mine(refused), "0123"); h.Refused != "" {
			t.Errorf("another head's refusal was read: %+v", h)
		}
		forged := []github.Comment{{Author: "someone", Body: refused}}
		if h := history(t, forged, sha); h.Refused != "" {
			t.Errorf("someone else's refusal was read: %+v", h)
		}
	})
	t.Run("the hand-off says how to answer it, verbatim", func(t *testing.T) {
		comment := HandOffComment("fix(deps): bump axios from 1.1.0 to 2.0.0", "PASS", sha)
		for _, want := range []string{"Approve this PR (GitHub review) to let the daemon land it; close to reject.", HandOffMarker(sha), "from 1.1.0 to 2.0.0", "CI green on " + sha} {
			if !strings.Contains(comment, want) {
				t.Errorf("the hand-off lacks %q:\n%s", want, comment)
			}
		}
	})
}

func TestReviewOf(t *testing.T) {
	const head, old = "headsha", "oldsha"
	review := func(who, state, commit string) github.Review {
		return github.Review{Author: who, State: state, CommitID: commit}
	}
	cases := []struct {
		name    string
		reviews []github.Review
		want    Reviewed
	}{
		{"no review", nil, Reviewed{}},
		{"an approval of the head", []github.Review{review("human", "APPROVED", head)}, Reviewed{Approved: true}},
		{"an approval of an older head vouches for nothing", []github.Review{review("human", "APPROVED", old)}, Reviewed{}},
		{"a request for changes stands on any head", []github.Review{review("human", "CHANGES_REQUESTED", old)}, Reviewed{ChangesRequested: true}},
		{"a reviewer's last review stands", []github.Review{review("human", "CHANGES_REQUESTED", old), review("human", "APPROVED", head)}, Reviewed{Approved: true}},
		{"a comment changes nothing", []github.Review{review("human", "APPROVED", head), review("human", "COMMENTED", head)}, Reviewed{Approved: true}},
		{"a dismissed review stands for nothing", []github.Review{review("human", "APPROVED", head), review("human", "DISMISSED", head)}, Reviewed{}},
		{"one reviewer's request outweighs another's approval", []github.Review{review("human", "APPROVED", head), review("Other", "CHANGES_REQUESTED", head)}, Reviewed{ChangesRequested: true}},
		{"the Daemon's own approval is not a human's", []github.Review{review("Agent-Bot", "APPROVED", head)}, Reviewed{}},
		{"Dependabot's is not either", []github.Review{review(DependabotLogin, "APPROVED", head)}, Reviewed{}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReviewOf(tt.reviews, "agent-bot", head); got != tt.want {
				t.Errorf("ReviewOf = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The bump sizes of lib/pr-triage.sh's _pr_triage_deps_major.
func TestMajorBump(t *testing.T) {
	for message, major := range map[string]bool{
		"chore(deps): bump axios from 1.1.0 to 1.1.2":                                                 false,
		"chore(deps): bump axios from 1.1.0 to 1.2.0":                                                 false,
		"chore(deps): bump axios from 1.9.3 to 2.0.0":                                                 true,
		"chore(deps): bump left-pad from 0.4.1 to 0.5.0":                                              true,
		"chore(deps): bump left-pad from 0.4.1 to 0.4.2":                                              false,
		"chore(deps): bump the npm group\n\nBumps a from 1.0.0 to 1.0.1\nBumps b from 3.2.0 to 4.0.0": true,
		"chore(deps): bump the npm group\n\nBumps a from 1.0.0 to 1.0.1\nBumps b from 3.2.0 to 3.3.0": false,
		"chore(deps): bump the npm group":                                                             true,
		"chore(deps): bump go from 1.21 to 1.22":                                                      false,
		"chore(deps): bump actions/checkout from 4 to 5":                                              true,
		"chore(deps): bump x from 0 to 0.1":                                                           true,
	} {
		if got := MajorBump(message); got != major {
			t.Errorf("MajorBump(%q) = %v, want %v", message, got, major)
		}
	}
}

func TestUntickChecklist(t *testing.T) {
	ticked := "Bumps axios.\n\n- [x] not the checklist's\n\n" + ChecklistMarker + "\n\n## Manual verification\n\n- [x] the tests pass\n  - [X] nested\n- [ ] the app starts\n\n" + ChecklistEnd + "\n"
	want := "Bumps axios.\n\n- [x] not the checklist's\n\n" + ChecklistMarker + "\n\n## Manual verification\n\n- [ ] the tests pass\n  - [ ] nested\n- [ ] the app starts\n\n" + ChecklistEnd + "\n"
	if got := UntickChecklist(ticked); got != want {
		t.Errorf("UntickChecklist =\n%s\nwant\n%s", got, want)
	}
	if got := UntickChecklist(want); got != want {
		t.Errorf("an unticked checklist changed:\n%s", got)
	}
	if body := "- [x] no checklist here\n"; UntickChecklist(body) != body {
		t.Error("a body with no Bot-PR checklist changed")
	}
}
