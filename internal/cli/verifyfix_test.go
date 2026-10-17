package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/step/fix"
	"github.com/benjr70/auto-agent/internal/step/steptest"
	"github.com/benjr70/auto-agent/internal/step/verifier"
)

// fixAnswers is the fake Fix Step answering a failed-verification brief.
func fixAnswers(run string, result fix.Result) steptest.Reply {
	return steptest.Reply{Run: run, Lines: []string{
		steptest.Init("fix-1", "claude-opus", append([]string{"StructuredOutput"}, fix.Spec().Tools...), []string{"factory:fix"}),
		steptest.Success(result),
	}}
}

// disputes is the fake Fix Step disputing the acceptance criterion itself.
func disputes(why string) steptest.Reply {
	return fixAnswers("echo stray > stray.txt", fix.Result{Summary: "the criterion is wrong", CommitMessage: "fix(a): nothing", Dispute: why})
}

func fails(text, evidence string) verifier.Verdict {
	return verifier.Verdict{Text: text, Verdict: "FAIL", Class: verifier.Local, Evidence: []string{evidence}}
}

const reachProdUntagged = "the greeting reaches prod"

// demands is a deployed-env deferral demanding a post-deploy check.
func demands(text, spec string) verifier.Verdict {
	return verifier.Verdict{Text: text, Verdict: "DEFER", Class: verifier.DeployedEnv, Evidence: []string{}, SpecDemanded: spec}
}

// fixBrief is the brief the Fix Step was sent, call by call.
func (f *fireFixture) fixBrief(t *testing.T, call int) string {
	t.Helper()
	calls := f.fixer.Calls()
	if len(calls) <= call {
		t.Fatalf("the Fix Step was called %d time(s)", len(calls))
	}
	var input struct {
		Brief json.RawMessage `json:"brief"`
	}
	stdin := calls[call].Stdin
	if at := strings.Index(stdin, "{"); at < 0 || json.Unmarshal([]byte(stdin[at:]), &input) != nil {
		t.Fatalf("the Fix Step was sent %s", stdin)
	}
	return string(input.Brief)
}

// Fixing a failed verification item, within rounds.manual_verify. The rules
// lib/runbook-check.sh holds plugin/skills/afk-pickup/SKILL.md to are none of
// §6a.3's: its rule table holds the skill's machine lines, its chain of
// skills and its Ruling request, and those are ported where Go now keeps
// them. §6a.3's own cases are ported here under the names its prose gives
// them: never-force (a plain push), cap-from-config (rounds.manual_verify),
// exhaustion-park (draft, AFK:checks-failed and the ticket's comment),
// dispute-park (manual-verify-dispute is exhaustion at once) and
// staged-nothing (a Fix that changes nothing is not a fix). Dropped: the
// `fix(manual): round <M>` commit subject (the Fix writes its own
// conventional subject, checked against commit_scopes, as every Fix does),
// and the implementer's 2000-line diff (the Fix reads the branch itself).
func TestFireVerifyFix(t *testing.T) {
	t.Run("a failed item gets a Fix, CI, and a passing re-verify of the still-unticked items (behaviour 1); never-force", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedPR(sayHello, loud)
		f.verifier.Script(verdicts(nil, passes(sayHello), fails(loud, "`./greet` printed hello in lower case")), verdicts(nil, passes(loud)))
		f.fixer.Script(fixed("echo HELLO > greeting.txt", "fix(a): shout the greeting"))
		reads := f.ciReads
		f.mustFire(0)
		// The brief is the failed item's evidence, and nothing else.
		if brief := f.fixBrief(t, 0); brief != `{"kind":"failed-verification","failed":[{"item":"the greeting is loud","detail":"`+"`./greet` printed hello in lower case"+`"}]}` {
			t.Errorf("the Fix was briefed %s", brief)
		}
		// The fix is a commit on top of the PR's head: pushed, never forced.
		if log := git(t, f.dir, "--git-dir", f.remote, "log", "--format=%s", "feat/issue-20"); !strings.HasPrefix(log, "fix(a): shout the greeting\n") {
			t.Errorf("branch log %q", log)
		}
		// CI was waited on for the fix before the round ran again.
		if f.ciReads <= reads+1 {
			t.Errorf("CI was read %d time(s)", f.ciReads-reads)
		}
		// Round 2 is told only the item still unticked.
		if input := f.verifyInput(t); len(input.Items) != 1 || input.Items[0].Text != loud {
			t.Errorf("round 2 was told %+v", input.Items)
		}
		comments := strings.Join(f.pr.comments, "\n")
		if !strings.Contains(comments, "### Manual verification — round 1/3") || !strings.Contains(comments, "### Manual verification — round 2/3") ||
			!strings.Contains(f.pr.body, "- [x] "+sayHello) || !strings.Contains(f.pr.body, "- [x] "+loud) {
			t.Errorf("comments %v, body\n%s", f.pr.comments, f.pr.body)
		}
		record := f.record()
		v := verifyOf(record)
		if f.labelled() != "AFK:done" || v["round"] != 2.0 || v["fixRounds"] != 1.0 || v["done"] != true || record["phase"] != "done" {
			t.Errorf("labels %q, record %v", f.labelled(), record)
		}
		if namesOf(record) != "verify,fix,verify" {
			t.Errorf("steps %s", namesOf(record))
		}
	})
	t.Run("cap-from-config and exhaustion-park: the cap drafts and labels the PR and says on the ticket how many items still fail (behaviour 2)", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"manual_verify":2}}`)
		f.reviewedPR(sayHello, loud)
		f.verifier.Script(verdicts(nil, fails(sayHello, "printed hi"), fails(loud, "printed hello")), verdicts(nil, passes(sayHello), fails(loud, "printed hello")))
		f.fixer.Script(fixed("echo one > greeting.txt", "fix(a): try one"), fixed("echo two > greeting.txt", "fix(a): try two"))
		f.mustFire(0)
		if len(f.verifier.Calls()) != 2 || len(f.fixer.Calls()) != 1 {
			t.Errorf("%d rounds, %d Fixes", len(f.verifier.Calls()), len(f.fixer.Calls()))
		}
		if !strings.Contains(f.writes(), "POST /graphql") || !strings.Contains(strings.Join(f.pr.labels, ","), "AFK:checks-failed") {
			t.Errorf("labels %v, GitHub received\n%s", f.pr.labels, f.writes())
		}
		if last := f.comments[len(f.comments)-1].Body; !strings.Contains(last, "Manual verification on PR #55 exhausted 2 round(s): 1 item(s) still FAIL. Marked draft and labelled `AFK:checks-failed`") {
			t.Errorf("the ticket's last comment %q", last)
		}
		record := f.record()
		if v := verifyOf(record); v["exhausted"] != true || v["round"] != 2.0 || record["parkedPR"] != "AFK:checks-failed" || f.labelled() == "AFK:done" {
			t.Errorf("labels %q, record %v", f.labelled(), record)
		}
	})
	t.Run("at the cap a deferral still owed its check is counted as missing, not as a FAIL", func(t *testing.T) {
		f := newFireFixture(t, `{"commit_scopes":["a"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"rounds":{"manual_verify":1}}`)
		f.reviewedPR(sayHello, reachProdUntagged)
		f.verifier.Script(verdicts(nil, passes(sayHello), demands(reachProdUntagged, "GET https://prod/greeting gives hello")))
		f.mustFire(0)
		if len(f.fixer.Calls()) != 0 || !strings.Contains(strings.Join(f.pr.labels, ","), "AFK:checks-failed") {
			t.Errorf("%d Fixes, labels %v", len(f.fixer.Calls()), f.pr.labels)
		}
		if last := f.comments[len(f.comments)-1].Body; !strings.Contains(last, "exhausted 1 round(s): 0 item(s) still FAIL, 1 demanded post-deploy check(s) still missing.") {
			t.Errorf("the ticket's last comment %q", last)
		}
	})
	t.Run("dispute-park: a Fix disputing the criterion parks the PR at once, its changes thrown away (behaviour 3)", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedPR(loud)
		f.verifier.Script(verdicts(nil, fails(loud, "printed hello")))
		f.fixer.Script(disputes("the ticket asks for a quiet greeting"))
		before := f.remoteRef("refs/heads/feat/issue-20")
		f.mustFire(0)
		if len(f.verifier.Calls()) != 1 || f.remoteRef("refs/heads/feat/issue-20") != before {
			t.Errorf("%d rounds, the branch moved", len(f.verifier.Calls()))
		}
		if last := f.comments[len(f.comments)-1].Body; !strings.Contains(last, "the Fix disputes the acceptance criterion itself (the ticket asks for a quiet greeting)") || !strings.Contains(last, "1 item(s) still FAIL") {
			t.Errorf("the ticket's last comment %q", last)
		}
		if v := verifyOf(f.record()); v["dispute"] != "the ticket asks for a quiet greeting" || f.record()["parkedPR"] != "AFK:checks-failed" || !strings.Contains(strings.Join(f.pr.labels, ","), "AFK:checks-failed") {
			t.Errorf("labels %v, record %v", f.pr.labels, f.record())
		}
		if out := git(t, f.dir, "-C", f.state+"/clone", "status", "--porcelain"); out != "" {
			t.Errorf("the checkout kept %q", out)
		}
	})
	t.Run("staged-nothing: a Fix that changes nothing and disputes nothing is a failed attempt", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedPR(loud)
		f.verifier.Script(verdicts(nil, fails(loud, "printed hello")))
		f.fixer.Script(fixed("", "fix(a): nothing"))
		f.mustFire(1)
		if err := f.record()["error"].(string); !strings.Contains(err, "the Fix changed nothing for the failed verification items and disputed nothing (failed attempt 1 of 3)") {
			t.Errorf("record %v", f.record())
		}
		if len(f.verifier.Calls()) != 1 {
			t.Errorf("%d rounds", len(f.verifier.Calls()))
		}
	})
	t.Run("a demanded post-deploy spec is answered: the script committed, the item tagged, the next round justified", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedPR(sayHello, reachProdUntagged)
		f.verifier.Script(verdicts(nil, passes(sayHello), demands(reachProdUntagged, "GET https://prod/greeting gives hello")),
			verdicts(nil, demands(reachProdUntagged+" <!-- post-deploy: scripts/prod-greeting.sh -->", "scripts/prod-greeting.sh")))
		f.fixer.Script(fixAnswers("mkdir -p scripts && echo 'curl -s https://prod/greeting' > scripts/prod-greeting.sh", fix.Result{
			Summary: "added the post-deploy check", CommitMessage: "test(a): check the greeting in prod",
			PostDeploy: []fix.PostDeploy{{Spec: 1, Check: "scripts/prod-greeting.sh"}},
		}))
		f.mustFire(0)
		if brief := f.fixBrief(t, 0); brief != `{"kind":"failed-verification","specs":[{"id":1,"item":"the greeting reaches prod","spec":"GET https://prod/greeting gives hello"}]}` {
			t.Errorf("the Fix was briefed %s", brief)
		}
		if !strings.Contains(f.pr.body, "- [ ] "+reachProdUntagged+" <!-- post-deploy: scripts/prod-greeting.sh -->\n") {
			t.Errorf("the body is\n%s", f.pr.body)
		}
		if files := git(t, f.dir, "--git-dir", f.remote, "ls-tree", "--name-only", "-r", "feat/issue-20"); !strings.Contains(files, "scripts/prod-greeting.sh") {
			t.Errorf("the branch holds %q", files)
		}
		if input := f.verifyInput(t); len(input.Items) != 1 || input.Items[0].PostDeploy != "scripts/prod-greeting.sh" {
			t.Errorf("round 2 was told %+v", input.Items)
		}
		if f.labelled() != "AFK:done" || verifyOf(f.record())["done"] != true {
			t.Errorf("labels %q, record %v", f.labelled(), f.record())
		}
	})
	t.Run("a spec answered with the checklist alone runs the round again with no push", func(t *testing.T) {
		f := newFireFixture(t, labelsConfig)
		f.reviewedPR(reachProdUntagged)
		f.verifier.Script(verdicts(nil, demands(reachProdUntagged, "GET https://prod/greeting gives hello")),
			verdicts(nil, demands(reachProdUntagged+" <!-- post-deploy: curl -s https://prod/greeting -->", "curl -s https://prod/greeting")))
		f.fixer.Script(fixAnswers("", fix.Result{Summary: "named the check", CommitMessage: "test(a): check prod",
			PostDeploy: []fix.PostDeploy{{Spec: 1, Check: "curl -s https://prod/greeting"}}}))
		before := f.remoteRef("refs/heads/feat/issue-20")
		f.mustFire(0)
		if f.remoteRef("refs/heads/feat/issue-20") != before || len(f.verifier.Calls()) != 2 || f.labelled() != "AFK:done" {
			t.Errorf("%d rounds, labels %q, record %v", len(f.verifier.Calls()), f.labelled(), f.record())
		}
	})
}
