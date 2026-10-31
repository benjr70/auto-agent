package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/github/githubtest"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step/verifier"
	"github.com/benjr70/auto-agent/internal/verify/providertest"
)

const (
	depsConfig = `{"commit_scopes":["a","deps"],"commands":{"install":"x","test":"y"},"pick":{"labels":{}},"dependabot":{},"rounds":{"deps_fix":2}}`
	botBranch  = "dependabot/npm_and_yarn/axios-1.1.2"
	botPath    = "/repos/acme/widgets/pulls/60"
	// botChecklist is the Target Project's bot-pr-checklist.md.
	botItem      = "The tests pass after the bump"
	botChecklist = "## Manual verification\n\n- [ ] " + botItem + "\n"
	botBody      = "Bumps axios from 1.1.0 to 1.1.2."
	// botMessage is Dependabot's first commit message: what the deps gate
	// sizes the bump from.
	botMessage = "chore(deps): bump axios from 1.1.0 to 1.1.2\n\nBumps [axios](https://github.com/axios/axios) from 1.1.0 to 1.1.2."
)

// botWorld is Bot PR #60 as the fake GitHub keeps it.
type botWorld struct {
	f        *fireFixture
	author   string
	title    string
	body     string
	draft    bool
	labels   []string
	comments []ticketComment
	// mergeable is what the PR read alone says: true, or false for a
	// conflict.
	mergeable bool
	// commits are the PR's commits as GitHub lists them, by author login.
	commits []string
	// failComments makes GitHub refuse to list its comments, once okReads
	// reads of them have been answered.
	failComments bool
	okReads      int
	commentReads int
	// headRepo is where its branch lives.
	headRepo string
	// noSHA lists it with no head sha.
	noSHA bool
	// message is the first commit's message.
	message string
	// reviews are its reviews as GitHub lists them.
	reviews []map[string]any
	// merged is the commit it landed as, and mergedAt the sha the merge
	// was pinned to; empty until it is merged.
	merged, mergedAt string
	// moveAtMerge pushes to its branch as the merge is asked for: a commit
	// pushed after the gate.
	moveAtMerge bool
	// afterTierB runs once Tier B's marker is posted.
	afterTierB func()
	// refuseMerge makes GitHub refuse every merge, as branch protection
	// does.
	refuseMerge bool
}

// newBotFixture is a Fire on a Target Project whose Harness config is
// harness, with a Bot-PR checklist on its default branch and Dependabot's
// branch on the remote, one bump commit ahead.
func newBotFixture(t *testing.T, harness string) (*fireFixture, *botWorld) {
	t.Helper()
	f := newFireFixture(t, harness)
	// No ticket waits: a Fire that takes no PR does nothing.
	f.queue = []githubtest.Reply{queueReply()}
	os.WriteFile(filepath.Join(f.dir, ".auto-agent", "bot-pr-checklist.md"), []byte(botChecklist), 0o644)
	git(t, f.dir, "add", "-A")
	git(t, f.dir, "commit", "--quiet", "-m", "the Bot-PR checklist")
	git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
	git(t, f.dir, "checkout", "--quiet", "-b", botBranch)
	os.WriteFile(filepath.Join(f.dir, "package-lock.json"), []byte("axios 1.1.2\n"), 0o644)
	git(t, f.dir, "add", "-A")
	git(t, f.dir, "-c", "user.name=dependabot[bot]", "-c", "user.email=49699333+dependabot[bot]@users.noreply.github.com", "commit", "--quiet", "-m", "chore(deps): bump axios from 1.1.0 to 1.1.2")
	git(t, f.dir, "push", "--quiet", f.remote, "HEAD:refs/heads/"+botBranch)
	git(t, f.dir, "checkout", "--quiet", "trunk")
	w := &botWorld{f: f, headRepo: "acme/widgets", author: state.DependabotLogin, title: "chore(deps): bump axios from 1.1.0 to 1.1.2", body: botBody, mergeable: true, commits: []string{state.DependabotLogin}, message: botMessage}
	w.serve()
	// Tier B: the one item of the Bot-PR checklist passes.
	f.verifier.Script(verdicts(nil, passes(botItem)))
	return f, w
}

// push puts a commit nobody in the Lane made on the Bot PR's branch.
func (w *botWorld) push() string {
	t, remote, head := w.f.t, w.f.remote, w.head()
	commit := git(t, remote, "commit-tree", head+"^{tree}", "-p", head, "-m", "a push after the gate")
	git(t, remote, "update-ref", "refs/heads/"+botBranch, commit)
	return commit
}

// approve is a human's approving review of the PR at sha.
func (w *botWorld) approve(sha string) {
	w.reviews = append(w.reviews, map[string]any{"state": "APPROVED", "commit_id": sha, "user": map[string]string{"login": "the-human"}})
}

// head is the Bot PR's head, as the remote has it.
func (w *botWorld) head() string { return w.f.remoteRef("refs/heads/" + botBranch) }

func (w *botWorld) json(mergeable bool) map[string]any {
	labels := []map[string]string{}
	for _, l := range w.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	pr := map[string]any{
		"number": 60, "node_id": "PR_60", "title": w.title, "body": w.body, "state": "open", "draft": w.draft, "created_at": "2026-10-01T09:00:00Z",
		"user": map[string]any{"login": w.author}, "labels": labels,
		"head": map[string]any{"ref": botBranch, "sha": w.head(), "repo": map[string]any{"full_name": w.headRepo}},
		"base": map[string]any{"ref": "trunk"},
	}
	if w.merged != "" {
		pr["state"], pr["merged"], pr["merged_at"], pr["merge_commit_sha"] = "closed", true, "2026-10-01T10:00:00Z", w.merged
	}
	if mergeable {
		pr["mergeable"] = w.mergeable
	}
	return pr
}

func (w *botWorld) serve() {
	gh := w.f.gh
	gh.HandleFunc("GET", pullsPath, func(githubtest.Request) githubtest.Reply {
		if w.merged != "" {
			return githubtest.JSON(200, []map[string]any{})
		}
		listed := w.json(false)
		if w.noSHA {
			listed["head"].(map[string]any)["sha"] = ""
		}
		return githubtest.JSON(200, []map[string]any{listed})
	})
	gh.HandleFunc("GET", botPath, func(githubtest.Request) githubtest.Reply { return githubtest.JSON(200, w.json(true)) })
	gh.HandleFunc("PATCH", botPath, func(r githubtest.Request) githubtest.Reply {
		var sent map[string]string
		json.Unmarshal([]byte(r.Body), &sent)
		if title, ok := sent["title"]; ok {
			w.title = title
		}
		if body, ok := sent["body"]; ok {
			w.body = body
		}
		return githubtest.JSON(200, map[string]any{"number": 60})
	})
	gh.HandleFunc("GET", botPath+"/commits", func(githubtest.Request) githubtest.Reply {
		list := []map[string]any{}
		for i, login := range w.commits {
			message := "fix(deps): a later commit"
			if i == 0 {
				message = w.message
			}
			list = append(list, map[string]any{"sha": "c", "author": map[string]any{"login": login}, "commit": map[string]any{"author": map[string]any{"name": login, "email": login + "@example.com"}, "message": message}})
		}
		return githubtest.JSON(200, list)
	})
	gh.HandleFunc("GET", botPath+"/reviews", func(githubtest.Request) githubtest.Reply {
		return githubtest.JSON(200, append([]map[string]any{}, w.reviews...))
	})
	gh.HandleFunc("PUT", botPath+"/merge", func(r githubtest.Request) githubtest.Reply {
		var sent struct {
			SHA   string `json:"sha"`
			Title string `json:"commit_title"`
		}
		json.Unmarshal([]byte(r.Body), &sent)
		if w.moveAtMerge {
			w.push()
		}
		if w.refuseMerge {
			return githubtest.JSON(405, map[string]any{"message": "Repository rule violations found"})
		}
		if sent.SHA != w.head() {
			return githubtest.JSON(409, map[string]any{"message": "Head branch was modified. Review and try the merge again."})
		}
		w.merged, w.mergedAt = "5ca1ab1e5ca1ab1e5ca1ab1e5ca1ab1e5ca1ab1e", sent.SHA
		return githubtest.JSON(200, map[string]any{"sha": w.merged, "merged": true})
	})
	gh.HandleFunc("GET", "/repos/acme/widgets/issues/60/comments", func(githubtest.Request) githubtest.Reply {
		w.commentReads++
		if w.failComments && w.commentReads > w.okReads {
			return githubtest.Text(502, "bad gateway")
		}
		return githubtest.JSON(200, w.comments)
	})
	gh.HandleFunc("POST", "/repos/acme/widgets/issues/60/comments", func(r githubtest.Request) githubtest.Reply {
		var sent struct{ Body string }
		json.Unmarshal([]byte(r.Body), &sent)
		w.comment("machine-user", sent.Body)
		if strings.Contains(sent.Body, "tierB=PASS") && w.afterTierB != nil {
			w.afterTierB()
		}
		return githubtest.JSON(201, map[string]any{"id": len(w.comments)})
	})
	gh.HandleFunc("POST", "/repos/acme/widgets/issues/60/labels", func(r githubtest.Request) githubtest.Reply {
		var sent struct{ Labels []string }
		json.Unmarshal([]byte(r.Body), &sent)
		w.labels = append(w.labels, sent.Labels...)
		return githubtest.Text(200, `[]`)
	})
}

func (w *botWorld) comment(author, body string) {
	w.comments = append(w.comments, ticketComment{ID: 6000 + len(w.comments), Body: body, User: map[string]string{"login": author}})
}

// posted are the bodies of the comments the Daemon posted on the Bot PR.
func (w *botWorld) posted() []string {
	var bodies []string
	for _, c := range w.comments {
		if c.User["login"] == "machine-user" {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

func depsOf(record map[string]any) map[string]any {
	deps, _ := record["deps"].(map[string]any)
	return deps
}

// reads are the GET requests GitHub was sent for path.
func (f *fireFixture) reads(path string) int {
	n := 0
	for _, r := range f.gh.Requests() {
		if r.Method == "GET" && r.Path == path {
			n++
		}
	}
	return n
}

// The Bot PR Lane: a Dependabot PR taken in the reconcile Lane, proved by
// Tier A and Tier B, and merged through the deps gate. With the triage
// cases the Fire runs
// (lib/pr-triage.test.sh) and the park and the cap
// (lib/deps-lane.test.sh), under their names.
func TestFireBotPR(t *testing.T) {
	t.Run("a patch bump with green CI is retitled, has its checklist, passes both tiers and is merged with the sha pinned (behaviour 1)", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		head := w.head()
		f.mustFire(0)
		record := f.record()
		deps := depsOf(record)
		if record["reconcile"] != "dependabot" || record["action"] != "deps-land" || record["issue"] != nil || record["pr"] != 60.0 || record["phase"] != "done" || record["error"] != "" {
			t.Errorf("record %v", record)
		}
		if deps["outcome"] != "merged" || deps["tierA"] != "green" || deps["tierB"] != "1/1" || deps["major"] != false || deps["gate"] != "approved at "+head || deps["merged"] != w.merged ||
			deps["security"] != true || deps["retitled"] != true || deps["checklist"] != true || deps["head"] != head {
			t.Errorf("deps %v", deps)
		}
		// Merged at the head both tiers vouched for, titled as GitHub
		// titles a squash.
		if w.mergedAt != head || !strings.Contains(f.writes(), "PUT "+botPath+"/merge") {
			t.Errorf("merged at %q, GitHub received\n%s", w.mergedAt, f.writes())
		}
		if w.title != "fix(deps): bump axios from 1.1.0 to 1.1.2" || !strings.HasPrefix(w.body, botBody+"\n\n"+state.ChecklistMarker) || !strings.Contains(w.body, "- [x] "+botItem) {
			t.Errorf("title %q, body %q", w.title, w.body)
		}
		// Tier A's marker, the round's evidence, Tier B's marker.
		posted := w.posted()
		if len(posted) != 3 || !strings.Contains(posted[0], "<!-- deps-lane tierA=green sha="+head+" -->") || !strings.Contains(posted[1], "### Manual verification\n") ||
			!strings.Contains(posted[2], "<!-- deps-lane tierB=PASS sha="+head+" -->") || !strings.Contains(posted[2], "manual-verify: 1/1 PASS, 0 deferred, 0 FAIL") {
			t.Errorf("comments %q", posted)
		}
		// The round is one forced-tour round over the Bot-PR checklist,
		// with no ticket, in the environment of PR #60.
		input := f.verifyInput(t)
		if len(f.verifier.Calls()) != 1 || len(input.Items) != 1 || input.Items[0].Text != botItem || input.Issue.Number != 0 || input.PR != 60 || input.Branch != botBranch {
			t.Errorf("the Verify Step was told %+v", input)
		}
		if calls := strings.Join(f.provider.Calls(), "|"); calls != "down --all|down --all|up --pr 60|down --pr 60" {
			t.Errorf("the provider was called %s", calls)
		}
		// No ticket: no lock, nothing on any issue but the PR's own.
		if writes := f.writes(); strings.Contains(writes, lockPath) || strings.Contains(writes, "/issues/20") {
			t.Errorf("GitHub received\n%s", writes)
		}
	})
	t.Run("dependabot.yml is looked up in the Target Project: a version bump keeps its chore title", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		git(t, f.dir, "checkout", "--quiet", "trunk")
		os.MkdirAll(filepath.Join(f.dir, ".github"), 0o755)
		os.WriteFile(filepath.Join(f.dir, ".github", "dependabot.yml"), []byte("version: 2\n"), 0o644)
		git(t, f.dir, "add", "-A")
		git(t, f.dir, "commit", "--quiet", "-m", "dependabot version updates")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		f.mustFire(0)
		if deps := depsOf(f.record()); deps["security"] != false || deps["retitled"] != false || w.title != "chore(deps): bump axios from 1.1.0 to 1.1.2" {
			t.Errorf("deps %v, title %q", deps, w.title)
		}
	})
	t.Run("park drafts the PR, labels it AFK:deps-failed and comments once: red CI twice with rounds.deps_fix 2, two Fix commits with the trailer, then parked (behaviour 2)", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.redCI()
		f.fixer.Script(fixed("echo one > package-lock.json", "fix(deps): regenerate the lockfile"), fixed("echo two > package-lock.json", "fix(deps): regenerate it again"))
		f.mustFire(0)
		record := f.record()
		if deps := depsOf(record); len(f.fixer.Calls()) != 2 || deps["outcome"] != "parked" || deps["tierA"] != "failed" || deps["attempts"] != 2.0 || record["parkedPR"] != "AFK:deps-failed" || record["phase"] != "deps-failed" || record["error"] != "" {
			t.Errorf("%d Fixes, record %v", len(f.fixer.Calls()), record)
		}
		log := git(t, f.dir, "--git-dir", f.remote, "log", "--format=%B%x00", "trunk.."+botBranch)
		if commits := strings.Split(strings.TrimSuffix(strings.TrimSpace(log), "\x00"), "\x00"); len(commits) != 3 ||
			strings.TrimSpace(commits[0]) != "fix(deps): regenerate it again [dependabot skip]" || strings.TrimSpace(commits[1]) != "fix(deps): regenerate the lockfile [dependabot skip]" {
			t.Errorf("branch log %q", log)
		}
		// The draft, the label and one comment carrying the park's marker.
		if !strings.Contains(f.writes(), "POST /graphql") || strings.Join(w.labels, ",") != "AFK:deps-failed" {
			t.Errorf("labels %v, GitHub received\n%s", w.labels, f.writes())
		}
		posted := w.posted()
		head := w.head()
		if len(posted) != 3 || strings.Count(posted[1], "sha="+head+" -->") != 2 || !strings.Contains(posted[2], state.ParkMarker(head)) || !strings.Contains(posted[2], "Last failure: CI still failing: unit") {
			t.Errorf("comments %q", posted)
		}
		// The budget follows the head: two attempts are counted on it.
		if h, _ := state.ReadDepsHistory(commentsOf(w.comments), "machine-user", head); h.Attempts != 2 {
			t.Errorf("history %+v", h)
		}
	})
	t.Run("a Fix the CI answers is Tier A fixed(1), its marker on the pushed head", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.redCI(green)
		f.fixer.Script(fixed("echo fixed > package-lock.json", "fix(deps): regenerate the lockfile"))
		f.mustFire(0)
		head := w.head()
		if deps := depsOf(f.record()); deps["tierA"] != "fixed(1)" || deps["outcome"] != "tier-a-green" || deps["attempts"] != 1.0 {
			t.Errorf("deps %v", deps)
		}
		if posted := w.posted(); len(posted) != 2 || !strings.Contains(posted[0], state.FixAttemptMarker(1, head)) || !strings.Contains(posted[1], "tierA=green sha="+head) {
			t.Errorf("comments %q", posted)
		}
		// The Fix was told the bump, with no ticket and no credential.
		call := f.fixer.Calls()[0]
		if !strings.Contains(call.Stdin, `"bump":{"pr":60`) || !strings.Contains(call.Stdin, `"issue":{"number":0`) || call.Env["GH_TOKEN"] != "" {
			t.Errorf("the Fix was given %s, env GH_TOKEN %q", call.Stdin, call.Env["GH_TOKEN"])
		}
	})
	t.Run("the fix cap is the Harness config's rounds.deps_fix", func(t *testing.T) {
		f, w := newBotFixture(t, strings.Replace(depsConfig, `"deps_fix":2`, `"deps_fix":1`, 1))
		f.redCI()
		f.fixer.Script(fixed("echo one > package-lock.json", "fix(deps): regenerate the lockfile"))
		f.mustFire(0)
		if deps := depsOf(f.record()); len(f.fixer.Calls()) != 1 || deps["cap"] != 1.0 || deps["outcome"] != "parked" || !strings.Contains(w.posted()[1], "fix budget of 1 fix attempts") {
			t.Errorf("%d Fixes, deps %v, comments %q", len(f.fixer.Calls()), deps, w.posted())
		}
	})
	t.Run("a bump with its budget spent has its CI read first: the last Fix is credited when it worked", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		comment, _ := state.FixAttemptsComment(1, 2, 2, w.head())
		w.comment("machine-user", comment)
		f.mustFire(0)
		if deps := depsOf(f.record()); f.ciReads == 0 || len(f.fixer.Calls()) != 0 || deps["outcome"] != "merged" || deps["attempts"] != 2.0 || !strings.Contains(w.posted()[1], "tierA=green sha="+w.head()) {
			t.Errorf("%d CI reads, deps %v", f.ciReads, deps)
		}
	})
	t.Run("a bump with its budget spent and CI still red is parked with no Fix", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.redCI()
		comment, _ := state.FixAttemptsComment(1, 2, 2, w.head())
		w.comment("machine-user", comment)
		f.mustFire(0)
		if deps := depsOf(f.record()); f.ciReads == 0 || len(f.fixer.Calls()) != 0 || deps["outcome"] != "parked" || !strings.Contains(w.posted()[1], "Last failure: CI still failing: unit") {
			t.Errorf("%d CI reads, deps %v, comments %q", f.ciReads, deps, w.posted())
		}
	})
	t.Run("park on a PR a human made ready again makes only the moves still owed", func(t *testing.T) {
		// A human made the PR ready again, the label off, on the same head:
		// it is parked again, and the comment it has already is not posted
		// twice.
		f, w := newBotFixture(t, depsConfig)
		f.redCI()
		comment, _ := state.FixAttemptsComment(1, 2, 2, w.head())
		w.comment("machine-user", comment)
		w.comment("machine-user", state.DepsParkedComment(2, "earlier", w.head()))
		f.mustFire(0)
		if writes := f.writes(); !strings.HasSuffix(writes, "\nPOST /graphql\nPOST /repos/acme/widgets/issues/60/labels") || len(w.posted()) != 2 {
			t.Errorf("GitHub received\n%s", writes)
		}
	})
	t.Run("park on an already-parked PR mutates nothing", func(t *testing.T) {
		// Parked by another hand between the Lane's read and its park: the
		// draft, the label and the comment are all there already.
		f, w := newBotFixture(t, depsConfig)
		f.redCI()
		comment, _ := state.FixAttemptsComment(1, 2, 2, w.head())
		w.comment("machine-user", comment)
		w.comment("machine-user", state.DepsParkedComment(2, "earlier", w.head()))
		w.title = "fix(deps): bump axios from 1.1.0 to 1.1.2"
		w.body, _ = state.InjectChecklist(botBody, botChecklist)
		reads := 0
		f.gh.HandleFunc("GET", botPath, func(githubtest.Request) githubtest.Reply {
			// The triage's read and the Lane's: then the park's.
			if reads++; reads > 2 {
				w.draft, w.labels = true, []string{state.LabelDepsFailed}
			}
			return githubtest.JSON(200, w.json(true))
		})
		f.mustFire(0)
		if deps := depsOf(f.record()); deps["outcome"] != "parked" || reads != 3 || len(f.gh.Writes()) != 0 || len(w.posted()) != 2 {
			t.Errorf("deps %v, %d PR reads, GitHub received\n%s", deps, reads, f.writes())
		}
	})
	t.Run("a marker for another sha counts for nothing", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		const old = "0123456789abcdef0123456789abcdef01234567"
		tierA, _ := state.TierAComment(old)
		spent, _ := state.FixAttemptsComment(1, 2, 2, old)
		w.comment("machine-user", tierA)
		w.comment("machine-user", spent)
		f.mustFire(0)
		if deps := depsOf(f.record()); f.ciReads == 0 || deps["attempts"] != 0.0 || deps["outcome"] != "merged" || !strings.Contains(w.posted()[2], "tierA=green sha="+w.head()) {
			t.Errorf("%d CI reads, deps %v", f.ciReads, deps)
		}
	})
	t.Run("a failed comment read never refunds the fix budget: it stops the lane", func(t *testing.T) {
		// The triage read them; the Lane's own read fails.
		f, w := newBotFixture(t, depsConfig)
		w.failComments, w.okReads = true, 1
		f.mustFire(1)
		if record := f.record(); !strings.Contains(record["error"].(string), "its fix attempts cannot be counted") || f.ciReads != 0 || len(f.fixer.Calls()) != 0 || len(f.gh.Writes()) != 0 {
			t.Errorf("record %v, GitHub received\n%s", record, f.writes())
		}
	})
	t.Run("inject reads the checklist path from the Harness config: with no bot-pr-checklist.md beside it the lane says so and stops", func(t *testing.T) {
		f, _ := newBotFixture(t, depsConfig)
		git(t, f.dir, "rm", "--quiet", ".auto-agent/bot-pr-checklist.md")
		git(t, f.dir, "commit", "--quiet", "-m", "no checklist")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		f.mustFire(1)
		if record := f.record(); !strings.Contains(record["error"].(string), "no bot-pr-checklist.md") || f.ciReads != 0 {
			t.Errorf("record %v", record)
		}
	})
	t.Run("a conflict with only Dependabot's commits: one @dependabot rebase comment, nothing pushed (behaviour 3)", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.mergeable = false
		head := w.head()
		f.mustFire(0)
		record := f.record()
		if record["reconcile"] != "conflict" || depsOf(record)["outcome"] != "nudged" || w.head() != head || f.ciReads != 0 {
			t.Errorf("record %v", record)
		}
		if writes := f.writes(); writes != "POST /repos/acme/widgets/issues/60/comments" || strings.Join(w.posted(), "|") != "@dependabot rebase" {
			t.Errorf("comments %q, GitHub received\n%s", w.posted(), writes)
		}
	})
	t.Run("a conflict on a branch the Daemon pushed to is rebased and pushed with a lease", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.mergeable, w.commits = false, []string{state.DependabotLogin, "machine-user"}
		git(t, f.dir, "checkout", "--quiet", "trunk")
		os.WriteFile(filepath.Join(f.dir, "other.txt"), []byte("moved on\n"), 0o644)
		git(t, f.dir, "add", "-A")
		git(t, f.dir, "commit", "--quiet", "-m", "trunk moves")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		head := w.head()
		f.mustFire(0)
		record := f.record()
		rebased := w.head()
		if depsOf(record)["outcome"] != "rebased" || rebased == head || record["commit"] != rebased || len(w.posted()) != 0 || f.ciReads != 0 {
			t.Errorf("record %v", record)
		}
		if base := git(t, f.dir, "--git-dir", f.remote, "merge-base", "trunk", botBranch); base != f.remoteRef("refs/heads/trunk") {
			t.Errorf("the branch is not on trunk: merge base %s", base)
		}
	})
	t.Run("a conflict a human must resolve is parked", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.mergeable, w.commits = false, []string{state.DependabotLogin, "machine-user"}
		git(t, f.dir, "checkout", "--quiet", "trunk")
		os.WriteFile(filepath.Join(f.dir, "package-lock.json"), []byte("axios 1.0.9\n"), 0o644)
		git(t, f.dir, "add", "-A")
		git(t, f.dir, "commit", "--quiet", "-m", "trunk touches the lockfile")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		head := w.head()
		f.mustFire(0)
		record := f.record()
		// It spent no fix attempt, and its comment does not say it did.
		if depsOf(record)["outcome"] != "parked" || w.head() != head || len(f.fixer.Calls()) != 0 || !strings.Contains(w.posted()[0], "no commands.lockfile_refresh (package-lock.json)") || strings.Contains(w.posted()[0], "fix budget") {
			t.Errorf("record %v, comments %q", record, w.posted())
		}
	})
	t.Run("a lockfile conflict goes to the Fix with the lockfile command", func(t *testing.T) {
		f, w := newBotFixture(t, strings.Replace(depsConfig, `"test":"y"`, `"test":"y","lockfile_refresh":"npm install --package-lock-only"`, 1))
		w.mergeable, w.commits = false, []string{state.DependabotLogin, "machine-user"}
		git(t, f.dir, "checkout", "--quiet", "trunk")
		os.WriteFile(filepath.Join(f.dir, "package-lock.json"), []byte("axios 1.0.9\n"), 0o644)
		git(t, f.dir, "add", "-A")
		git(t, f.dir, "commit", "--quiet", "-m", "trunk touches the lockfile")
		git(t, f.dir, "push", "--quiet", f.remote, "HEAD:trunk")
		f.fixer.Script(fixed("echo 'axios 1.1.2' > package-lock.json", "fix(deps): resolve the lockfile"))
		f.mustFire(0)
		if deps := depsOf(f.record()); deps["outcome"] != "rebased" || len(f.fixer.Calls()) != 1 || !strings.Contains(f.fixer.Calls()[0].Stdin, `"lockfile_refresh":"npm install --package-lock-only"`) {
			t.Errorf("deps %v", deps)
		}
	})
	t.Run("Dependabot impostors and parked shapes are refused", func(t *testing.T) {
		for name, change := range map[string]func(*botWorld){
			// behaviour 4: a dependabot/ branch opened by a human.
			"a human's dependabot/ branch":  func(w *botWorld) { w.author = "some-human" },
			"a fork's dependabot/ branch":   func(w *botWorld) { w.headRepo = "someone/widgets" },
			"a draft":                       func(w *botWorld) { w.draft = true },
			"AFK:deps-failed":               func(w *botWorld) { w.labels = []string{"AFK:deps-failed"} },
			"HITL with no approving review": func(w *botWorld) { w.labels = []string{"HITL"} },
		} {
			t.Run(name, func(t *testing.T) {
				f, w := newBotFixture(t, depsConfig)
				change(w)
				f.gh.Handle("GET", botPath+"/reviews", githubtest.JSON(200, []map[string]any{{"state": "CHANGES_REQUESTED", "user": map[string]string{"login": "the-human"}}}))
				f.mustFire(0)
				if record := f.record(); record["reconcile"] != "" || record["deps"] != nil || f.reads(botPath+"/commits") != 0 || len(f.gh.Writes()) != 0 {
					t.Errorf("record %v, GitHub received\n%s", record, f.writes())
				}
			})
		}
	})
	t.Run("HITL bot PR picked only once APPROVED", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.labels = []string{"HITL"}
		w.reviews = []map[string]any{{"state": "CHANGES_REQUESTED", "commit_id": w.head(), "user": map[string]string{"login": "the-human"}}}
		w.approve(w.head())
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "dependabot" {
			t.Errorf("record %v", record)
		}
	})
	t.Run("a HITL bot PR approved at an older head is not taken", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.labels = []string{"HITL"}
		w.approve("0123456789abcdef0123456789abcdef01234567")
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "" || len(f.gh.Writes()) != 0 {
			t.Errorf("record %v, GitHub received\n%s", record, f.writes())
		}
	})
	t.Run("bot verdicts are unworkable exactly when the deps-land lane is off: no Bot PR is taken, and the Fire record notes why", func(t *testing.T) {
		for config, why := range map[string]string{
			strings.Replace(depsConfig, `"dependabot":{},`, "", 1):                              "the Harness config declares no dependabot block",
			strings.Replace(depsConfig, `"dependabot":{}`, `"dependabot":{"enabled":false}`, 1): "dependabot.enabled is false",
		} {
			f, _ := newBotFixture(t, config)
			f.mustFire(0)
			record := f.record()
			want := fmt.Sprintf("Bot PR #60 is not taken: the Bot PR Lane is off (%s)", why)
			if warnings := fmt.Sprint(record["warnings"]); record["reconcile"] != "" || !strings.Contains(warnings, want) || f.reads(botPath+"/commits") != 0 || len(f.gh.Writes()) != 0 {
				t.Errorf("record %v", record)
			}
		}
	})
	t.Run("scan picks a Dependabot PR with its head sha", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "dependabot" || record["pr"] != 60.0 || depsOf(record)["head"] != w.head() {
			t.Errorf("record %v", record)
		}
	})
	t.Run("a listing without headRefOid costs no bot round trip", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.noSHA = true
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "" || f.reads(botPath) != 0 || f.reads(botPath+"/commits") != 0 || w.commentReads != 0 || len(f.gh.Writes()) != 0 {
			t.Errorf("record %v", record)
		}
	})
	t.Run("a broken Dependabot probe yields no dependabot pick", func(t *testing.T) {
		f, _ := newBotFixture(t, depsConfig)
		f.gh.Handle("GET", botPath, githubtest.Text(502, "bad gateway"))
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "" || len(f.gh.Writes()) != 0 || !strings.Contains(fmt.Sprint(record["warnings"]), "whether PR #60 merges could not be read") {
			t.Errorf("record %v", record)
		}
	})
	t.Run("an Agent PR is worked before a Bot PR, and the Bot PR is not read", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		agent := openPR(55, "feat/issue-20", "2026-10-12T09:00:00Z", "AFK:revise")
		f.gh.HandleFunc("GET", pullsPath, func(githubtest.Request) githubtest.Reply {
			return githubtest.JSON(200, []map[string]any{w.json(false), agent})
		})
		derived := triageDryRun(t, f)
		if !strings.Contains(derived, `"pr": 55`) || f.reads(botPath+"/commits") != 0 {
			t.Errorf("state %s", derived)
		}
	})
}

// merges are the merges GitHub was asked for.
func (f *fireFixture) merges() int { return strings.Count(f.writes(), "PUT "+botPath+"/merge") }

// The Bot PR Lane, second half: Tier B, the deps gate and the merge.
func TestFireBotPRTierB(t *testing.T) {
	t.Run("the head moved after Tier B: the gate refuses, nothing merged (behaviour 2)", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		gated := w.head()
		w.afterTierB = func() { w.push() }
		f.mustFire(0)
		deps := depsOf(f.record())
		if f.merges() != 0 || w.merged != "" || deps["outcome"] != "refused" || !strings.HasPrefix(deps["gate"].(string), "markers-stale: the PR is at "+w.head()+", not the "+gated) {
			t.Errorf("deps %v, GitHub received\n%s", deps, f.writes())
		}
		// A refusal the next head answers says nothing on the PR.
		if posted := w.posted(); len(posted) != 3 {
			t.Errorf("comments %q", posted)
		}
	})
	t.Run("a commit pushed between the gate and the merge is never merged: the merge is pinned to the gated sha", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		gated := w.head()
		w.moveAtMerge = true
		f.mustFire(0)
		deps := depsOf(f.record())
		if w.merged != "" || f.merges() != 1 || !strings.Contains(f.gh.Writes()[len(f.gh.Writes())-1].Body, `"sha":"`+gated+`"`) || deps["outcome"] != "refused" || !strings.HasPrefix(deps["gate"].(string), "merge-failed: ") {
			t.Errorf("deps %v, GitHub received\n%s", deps, f.writes())
		}
	})
	t.Run("a major bump passing both tiers is HITL with the hand-off, not merged; an approving review of its head has the same sha merged (behaviour 3)", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.title, w.message = "chore(deps): bump axios from 1.1.0 to 2.0.0", "chore(deps): bump axios from 1.1.0 to 2.0.0\n\nBumps axios from 1.1.0 to 2.0.0."
		head := w.head()
		f.mustFire(0)
		deps := depsOf(f.record())
		if f.merges() != 0 || deps["outcome"] != "hitl" || deps["major"] != true || !strings.HasPrefix(deps["gate"].(string), "major-unapproved") || strings.Join(w.labels, ",") != "HITL" {
			t.Errorf("deps %v, labels %v", deps, w.labels)
		}
		posted := w.posted()
		handOff := posted[len(posted)-1]
		for _, want := range []string{"fix(deps): bump axios from 1.1.0 to 2.0.0", "CI green on " + head, "manual-verify: 1/1 PASS", "## Screenshots",
			"Approve this PR (GitHub review) to let the daemon land it; close to reject.", state.HandOffMarker(head)} {
			if !strings.Contains(handOff, want) {
				t.Errorf("the hand-off lacks %q:\n%s", want, handOff)
			}
		}
		// Unapproved, it waits: the next Fire does not take it.
		before := len(f.gh.Writes())
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "" || len(f.gh.Writes()) != before {
			t.Errorf("record %v", record)
		}
		// Approved at its head: gated again on the same sha and merged,
		// with no tier run twice.
		w.approve(head)
		f.mustFire(0)
		deps = depsOf(f.record())
		if w.mergedAt != head || deps["outcome"] != "merged" || deps["tierA"] != "green" || deps["tierB"] != "skipped" || len(f.verifier.Calls()) != 1 || len(w.posted()) != len(posted) {
			t.Errorf("merged at %q, deps %v, %d rounds", w.mergedAt, deps, len(f.verifier.Calls()))
		}
	})
	t.Run("a Tier B item deferred is a failure: a Fix on the shared counter, pushed with the trailer and counted on the new head (behaviour 4)", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.verifier.Script(verdicts(nil, verifier.Verdict{Text: botItem, Verdict: "DEFER", Class: verifier.Hardware, Evidence: []string{}, Blocker: "the panel must be attached"}))
		f.fixer.Script(fixed("echo fixed > package-lock.json", "fix(deps): regenerate the lockfile"))
		f.mustFire(0)
		record := f.record()
		deps := depsOf(record)
		head := w.head()
		if f.merges() != 0 || deps["outcome"] != "tier-b-fixed" || deps["tierB"] != "0/1" || deps["attempts"] != 1.0 || record["commit"] != head || record["error"] != "" {
			t.Errorf("record %v", record)
		}
		if call := f.fixer.Calls(); len(call) != 1 || !strings.Contains(call[0].Stdin, `"kind":"failed-verification"`) || !strings.Contains(call[0].Stdin, "deferred, which fails Tier B: needs hardware: the panel must be attached") || !strings.Contains(call[0].Stdin, `"bump":{"pr":60`) {
			t.Errorf("the Fix was given %v", call)
		}
		if subject := git(t, f.dir, "--git-dir", f.remote, "log", "-1", "--format=%s", botBranch); subject != "fix(deps): regenerate the lockfile [dependabot skip]" {
			t.Errorf("the branch's head is %q", subject)
		}
		// Both tiers prove the new head again: its markers start empty.
		if h, _ := state.ReadDepsHistory(commentsOf(w.comments), "machine-user", head); h.Attempts != 1 || h.TierA || h.TierB {
			t.Errorf("history of the new head %+v", h)
		}
	})
	t.Run("a Tier B failure with the budget spent parks the PR", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		comment, _ := state.FixAttemptsComment(1, 2, 2, w.head())
		w.comment("machine-user", comment)
		f.verifier.Script(verdicts(nil, fails(botItem, "`npm test` failed: axios is not a function")))
		f.mustFire(0)
		deps := depsOf(f.record())
		posted := w.posted()
		if f.merges() != 0 || len(f.fixer.Calls()) != 0 || deps["outcome"] != "parked" || deps["tierB"] != "0/1 failed" || !strings.Contains(posted[len(posted)-1], "Last failure: Tier B: "+botItem) || strings.Join(w.labels, ",") != "AFK:deps-failed" {
			t.Errorf("deps %v, labels %v, comments %q", deps, w.labels, posted)
		}
	})
	t.Run("a Fix disputing the Bot-PR checklist parks the PR", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.verifier.Script(verdicts(nil, fails(botItem, "`npm test` failed")))
		f.fixer.Script(disputes("the checklist item tests nothing this bump touches"))
		head := w.head()
		f.mustFire(0)
		posted := w.posted()
		if deps := depsOf(f.record()); deps["outcome"] != "parked" || w.head() != head || !strings.Contains(posted[len(posted)-1], "the Fix disputes the checklist: the checklist item tests nothing") {
			t.Errorf("deps %v, comments %q", deps, posted)
		}
	})
	t.Run("a checklist ticked on an older head is verified again on the new one: no Tier B passes on old ticks", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		body, _ := state.InjectChecklist(botBody, botChecklist)
		w.body = strings.Replace(body, "- [ ] "+botItem, "- [x] "+botItem, 1)
		w.title = "fix(deps): bump axios from 1.1.0 to 1.1.2"
		f.verifier.Script(verdicts(nil, fails(botItem, "`npm test` failed")))
		f.fixer.Script(fixed("echo fixed > package-lock.json", "fix(deps): regenerate the lockfile"))
		f.mustFire(0)
		deps := depsOf(f.record())
		input := f.verifyInput(t)
		if f.merges() != 0 || deps["outcome"] != "tier-b-fixed" || deps["tierB"] != "0/1" || len(input.Items) != 1 || input.Items[0].Text != botItem {
			t.Errorf("deps %v, the Verify Step was told %+v", deps, input.Items)
		}
		for _, c := range w.posted() {
			if strings.Contains(c, "tierB=PASS") {
				t.Errorf("Tier B passed on old ticks: %q", c)
			}
		}
	})
	t.Run("a Bot-PR checklist emptied of items is no Tier B pass", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.title = "fix(deps): bump axios from 1.1.0 to 1.1.2"
		w.body = botBody + "\n\n" + state.ChecklistMarker + "\n\nnothing to check\n\n" + state.ChecklistEnd + "\n"
		f.mustFire(1)
		if record := f.record(); f.merges() != 0 || !strings.Contains(record["error"].(string), "verified no item") || strings.Contains(strings.Join(w.posted(), "\n"), "tierB=PASS") {
			t.Errorf("record %v", record)
		}
	})
	t.Run("an environment that does not boot is no Tier B verdict and costs no attempt", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.provider.Script(providertest.Script{Up: []providertest.Reply{{Exit: 4, Stderr: "compose: the db did not start"}}})
		f.mustFire(0)
		deps := depsOf(f.record())
		if f.merges() != 0 || len(f.verifier.Calls()) != 0 || len(f.fixer.Calls()) != 0 || deps["outcome"] != "tier-b-infra" || deps["tierB"] != "infra" || deps["attempts"] != 0.0 {
			t.Errorf("deps %v", deps)
		}
		if h, _ := state.ReadDepsHistory(commentsOf(w.comments), "machine-user", w.head()); h.Attempts != 0 || h.TierB {
			t.Errorf("history %+v", h)
		}
		// The next Fire runs the round again.
		f.provider.Script(providertest.Healthy("WEB_URL=http://127.0.0.1:9/\n"))
		f.mustFire(0)
		if deps := depsOf(f.record()); deps["outcome"] != "merged" || deps["tierA"] != "green" {
			t.Errorf("deps %v", deps)
		}
	})
	t.Run("the Bootstrap state hands the PR to a human without running Tier B", func(t *testing.T) {
		f, w := newBotFixture(t, strings.Replace(depsConfig, `"dependabot":{}`, `"dependabot":{},"verification":{}`, 1))
		f.mustFire(0)
		deps := depsOf(f.record())
		posted := w.posted()
		if f.merges() != 0 || len(f.verifier.Calls()) != 0 || deps["outcome"] != "verify-human" || deps["tierB"] != "skipped" || f.record()["parkedPR"] != "AFK:verify-human" ||
			strings.Join(w.labels, ",") != "AFK:verify-human" || !strings.Contains(f.writes(), "POST /graphql") || !strings.Contains(posted[len(posted)-1], "declares no hermetic tier") {
			t.Errorf("deps %v, labels %v, comments %q, GitHub received\n%s", deps, w.labels, posted, f.writes())
		}
	})
	t.Run("a Tier B verifier Step that keeps failing spends the shared budget, then parks the PR", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		f.verifier.Script(crashes)
		head := w.head()
		for attempt := 1; attempt <= 2; attempt++ {
			f.mustFire(1)
			record := f.record()
			if deps := depsOf(record); deps["attempts"] != float64(attempt) || !strings.Contains(record["error"].(string), "the verify Step ended FAILED") {
				t.Fatalf("Fire %d: record %v", attempt, record)
			}
			if h, _ := state.ReadDepsHistory(commentsOf(w.comments), "machine-user", head); h.Attempts != attempt {
				t.Fatalf("Fire %d: history %+v", attempt, h)
			}
		}
		f.mustFire(0)
		deps := depsOf(f.record())
		posted := w.posted()
		if f.merges() != 0 || len(f.fixer.Calls()) != 0 || deps["outcome"] != "parked" || strings.Join(w.labels, ",") != "AFK:deps-failed" || !strings.Contains(posted[len(posted)-1], "the verify Step ended FAILED") {
			t.Errorf("deps %v, labels %v, comments %q", deps, w.labels, posted)
		}
		// Parked, it is not taken again.
		before := len(f.gh.Writes())
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "" || len(f.gh.Writes()) != before {
			t.Errorf("record %v", record)
		}
	})
	t.Run("checks that turn red under both tier markers spend the shared budget, then park the PR", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		head := w.head()
		tierA, _ := state.TierAComment(head)
		tierB, _ := state.TierBComment(head, "manual-verify: 1/1 PASS")
		w.comment("machine-user", tierA)
		w.comment("machine-user", tierB)
		f.ci = [][]map[string]any{red}
		for attempt := 1; attempt <= 2; attempt++ {
			f.mustFire(0)
			deps := depsOf(f.record())
			if deps["outcome"] != "refused" || deps["attempts"] != float64(attempt) || !strings.HasPrefix(deps["gate"].(string), "checks-not-green") {
				t.Fatalf("Fire %d: deps %v", attempt, deps)
			}
		}
		f.mustFire(0)
		deps := depsOf(f.record())
		posted := w.posted()
		if f.merges() != 0 || len(f.verifier.Calls()) != 0 || deps["outcome"] != "parked" || strings.Join(w.labels, ",") != "AFK:deps-failed" || !strings.Contains(posted[len(posted)-1], "checks-not-green") {
			t.Errorf("deps %v, labels %v, comments %q", deps, w.labels, posted)
		}
	})
	t.Run("a check still pending under both tier markers costs nothing: the Fire ends waiting", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		head := w.head()
		tierA, _ := state.TierAComment(head)
		tierB, _ := state.TierBComment(head, "manual-verify: 1/1 PASS")
		w.comment("machine-user", tierA)
		w.comment("machine-user", tierB)
		f.ci = [][]map[string]any{{checkRun(1, "build", "in_progress", "")}}
		before := len(w.posted())
		for fire := 1; fire <= 3; fire++ {
			f.mustFire(0)
			deps := depsOf(f.record())
			if deps["outcome"] != "waiting-ci" || deps["attempts"] != float64(0) || !strings.HasPrefix(deps["gate"].(string), "checks-pending") {
				t.Fatalf("Fire %d: deps %v", fire, deps)
			}
		}
		if h, _ := state.ReadDepsHistory(commentsOf(w.comments), "machine-user", head); h.Attempts != 0 {
			t.Errorf("history %+v", h)
		}
		if f.merges() != 0 || len(f.verifier.Calls()) != 0 || len(w.labels) != 0 || len(w.posted()) != before {
			t.Errorf("labels %v, comments %q", w.labels, w.posted())
		}
	})
	t.Run("a merge GitHub keeps refusing spends the shared budget, then parks the PR", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.refuseMerge = true
		for attempt := 1; attempt <= 2; attempt++ {
			f.mustFire(0)
			deps := depsOf(f.record())
			if deps["outcome"] != "refused" || deps["attempts"] != float64(attempt) || !strings.HasPrefix(deps["gate"].(string), "merge-failed: ") {
				t.Fatalf("Fire %d: deps %v", attempt, deps)
			}
		}
		f.mustFire(0)
		deps := depsOf(f.record())
		posted := w.posted()
		if w.merged != "" || f.merges() != 3 || len(f.verifier.Calls()) != 1 || deps["outcome"] != "parked" || strings.Join(w.labels, ",") != "AFK:deps-failed" || !strings.Contains(posted[len(posted)-1], "the merge failed") {
			t.Errorf("deps %v, labels %v, comments %q", deps, w.labels, posted)
		}
	})
	t.Run("a refusal only a human answers is said once, and its head is not taken again", func(t *testing.T) {
		f, w := newBotFixture(t, depsConfig)
		w.title = "chore(deps-dev): bump jest from 29.0.0 to 29.1.0"
		w.message = "chore(deps-dev): bump jest from 29.0.0 to 29.1.0"
		head := w.head()
		f.mustFire(0)
		deps := depsOf(f.record())
		posted := w.posted()
		if f.merges() != 0 || deps["outcome"] != "refused" || !strings.HasPrefix(deps["gate"].(string), "title-not-deps") || !strings.Contains(posted[len(posted)-1], state.RefusedMarker("title-not-deps", head)) {
			t.Errorf("deps %v, comments %q", deps, posted)
		}
		before := len(f.gh.Writes())
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "" || len(f.gh.Writes()) != before || !strings.Contains(fmt.Sprint(record["warnings"]), state.RefusedWarning(60, "title-not-deps")) {
			t.Errorf("record %v, GitHub received\n%s", record, f.writes())
		}
		// A new head is taken again.
		w.push()
		f.mustFire(0)
		if record := f.record(); record["reconcile"] != "dependabot" {
			t.Errorf("record %v", record)
		}
	})
}

// commentsOf are the fake's comments as the client reads them.
func commentsOf(list []ticketComment) []github.Comment {
	comments := make([]github.Comment, len(list))
	for i, c := range list {
		comments[i] = github.Comment{ID: int64(c.ID), Author: c.User["login"], Body: c.Body}
	}
	return comments
}

// triageDryRun is what run --dry-run derives, as JSON.
func triageDryRun(t *testing.T, f *fireFixture) string {
	t.Helper()
	f.stdout.Reset()
	if code := f.run("run", "--once", "--dry-run", "--repo", "acme/widgets", f.dir); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, f.stderr.String())
	}
	return f.stdout.String()
}
