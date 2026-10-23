package state

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
)

// The Bot PR Lane's vocabulary, unchanged from the bash it replaces
// (lib/deps-lane.sh): a Bot PR's memory is the hidden markers the Daemon
// leaves in its comments, keyed to the head sha, so that every verdict
// expires the moment the PR moves.
const (
	// DependabotLogin is the Dependabot app as the REST API names the
	// author of its pull requests. No account can take the name: a login
	// never holds brackets.
	DependabotLogin = "dependabot[bot]"
	// DependabotPrefix starts every branch Dependabot opens.
	DependabotPrefix = "dependabot/"
	// DependabotRebase is the comment that asks Dependabot to rebase its
	// own branch.
	DependabotRebase = "@dependabot rebase"
	// ChecklistMarker and ChecklistEnd wrap the Bot-PR checklist in a Bot
	// PR's body: the marker is what makes the injection happen once.
	ChecklistMarker = "<!-- bot-pr-checklist v1 -->"
	ChecklistEnd    = "<!-- /bot-pr-checklist -->"
	// SkipTrailer ends every commit the Daemon pushes to a Bot PR's
	// branch: without it Dependabot takes the branch for a human's, and
	// stops rebasing it.
	SkipTrailer = "[dependabot skip]"
	// securityFooter is how Dependabot's body says the bump fixes a
	// vulnerability.
	securityFooter = "automated security fix"
)

// IsBotPR says a pull request is a Bot PR: opened by the Dependabot app on
// one of its branches. Either alone is not one: a human can name a branch
// dependabot/…, and the app's login on another branch is not its bump.
func IsBotPR(pr github.PullRequest) bool {
	return strings.EqualFold(pr.Author, DependabotLogin) && strings.HasPrefix(pr.HeadRef, DependabotPrefix)
}

// DepsLaneOff says why the Bot PR Lane is off; empty when it is on. It is
// on only when the Harness config declares a dependabot block that does
// not switch it off.
func DepsLaneOff(lane config.Lane) string {
	switch {
	case !lane.Present:
		return "the Harness config declares no dependabot block"
	case !lane.Enabled:
		return "dependabot.enabled is false"
	}
	return ""
}

// BotSignals are what the PR Triage reads about a Bot PR beyond the
// listing: what decides which Bot PR goes first, and how a conflict on one
// is answered.
type BotSignals struct {
	// Security: the bump fixes a vulnerability.
	Security bool
	// AgentCommits: the branch carries a commit Dependabot did not make,
	// so Dependabot would refuse to rebase it.
	AgentCommits bool
	// Approved: a human approved its head in a review, which a HITL one
	// waits on.
	Approved bool
}

// IsSecurity says a Bot PR is a security bump: its body carries
// Dependabot's footer, or the Target Project has no dependabot.yml, so that
// only security updates are ever opened.
func IsSecurity(body string, dependabotConfig bool) bool {
	return !dependabotConfig || strings.Contains(strings.ToLower(body), securityFooter)
}

// AgentCommits says a Bot PR's branch carries a commit Dependabot did not
// make: one none of whose authors names dependabot.
func AgentCommits(commits []github.Commit) bool {
	for _, c := range commits {
		who := strings.ToLower(c.Login + " " + c.Name + " " + c.Email)
		if !strings.Contains(who, "dependabot") {
			return true
		}
	}
	return false
}

// The markers' shapes.
var (
	fixAttemptMarker = regexp.MustCompile(`<!-- deps-lane fix-attempt=[0-9]+ sha=(\S+) -->`)
	refusedMarker    = regexp.MustCompile(`<!-- deps-lane refused=(\S+) sha=(\S+) -->`)
	shaShape         = regexp.MustCompile(`^\S+$`)
)

// The tier markers.
func tierAMarker(sha string) string { return fmt.Sprintf("<!-- deps-lane tierA=green sha=%s -->", sha) }
func tierBMarker(sha string) string { return fmt.Sprintf("<!-- deps-lane tierB=PASS sha=%s -->", sha) }

// HandOffMarker is what the comment handing a major bump at sha to a human
// carries: a second hand-off of the same head posts nothing.
func HandOffMarker(sha string) string { return fmt.Sprintf("<!-- deps-lane handoff sha=%s -->", sha) }

// RefusedMarker is what the comment saying the deps gate refused sha for a
// human's reason carries: the PR Triage leaves that head alone.
func RefusedMarker(reason, sha string) string {
	return fmt.Sprintf("<!-- deps-lane refused=%s sha=%s -->", reason, sha)
}

// FixAttemptMarker is the marker of the nth fix attempt on a Bot PR, at sha.
func FixAttemptMarker(n int, sha string) string {
	return fmt.Sprintf("<!-- deps-lane fix-attempt=%d sha=%s -->", n, sha)
}

// ParkMarker is what the comment parking a Bot PR at sha carries: a second
// park of the same head posts nothing.
func ParkMarker(sha string) string { return fmt.Sprintf("<!-- deps-lane parked sha=%s -->", sha) }

// errNoSHA refuses a marker for no head: an all-false history would be a
// lie about markers nobody looked for, and would hand the PR a full budget.
var errNoSHA = errors.New("a Bot PR marker needs the head sha")

// TierAComment is the comment that records Tier A green at sha.
func TierAComment(sha string) (string, error) {
	if !shaShape.MatchString(sha) {
		return "", errNoSHA
	}
	return "Tier A: CI is green on " + sha + ".\n\n" + tierAMarker(sha), nil
}

// TierBComment is the comment that records Tier B passed at sha, with the
// round's line.
func TierBComment(sha, line string) (string, error) {
	if !shaShape.MatchString(sha) {
		return "", errNoSHA
	}
	return "Tier B: every item of the Bot-PR checklist passed on " + sha + " (" + line + ").\n\n" + tierBMarker(sha), nil
}

// FixAttemptsComment is the comment that records fix attempts from to
// count at sha, one marker each. After a push it records them all: the push
// moved the head, and the history follows it, or the count would start
// again after every push. An attempt that pushed nothing records only
// itself, on the head that has the rest.
func FixAttemptsComment(from, count, cap int, sha string) (string, error) {
	if !shaShape.MatchString(sha) {
		return "", errNoSHA
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Fix attempt %d of %d on this bump.\n\n", count, cap)
	for n := max(from, 1); n <= count; n++ {
		b.WriteString(FixAttemptMarker(n, sha) + "\n")
	}
	return b.String(), nil
}

// DepsHistory is what a Bot PR's markers say of one head sha.
type DepsHistory struct {
	SHA   string
	TierA bool
	TierB bool
	// Refused is the reason the deps gate refused the head for, when it
	// was a human's to answer; empty otherwise.
	Refused string
	// Attempts are the fix attempts spent on the head: one per marker
	// carrying its sha. The number each carries is not read back, so one
	// marker cannot inflate the count.
	Attempts int
}

// ReadDepsHistory is what the Daemon's own markers in a Bot PR's comments
// say of sha. Markers for any other sha count for nothing, a prefix of it
// or one it is a prefix of included.
func ReadDepsHistory(comments []github.Comment, login, sha string) (DepsHistory, error) {
	if !shaShape.MatchString(sha) {
		return DepsHistory{}, errNoSHA
	}
	h := DepsHistory{SHA: sha}
	for _, c := range comments {
		if login != "" && !strings.EqualFold(c.Author, login) {
			// Anyone can write a marker; only the Daemon's count.
			continue
		}
		h.TierA = h.TierA || strings.Contains(c.Body, tierAMarker(sha))
		h.TierB = h.TierB || strings.Contains(c.Body, tierBMarker(sha))
		for _, m := range refusedMarker.FindAllStringSubmatch(c.Body, -1) {
			if m[2] == sha {
				h.Refused = m[1]
			}
		}
		for _, m := range fixAttemptMarker.FindAllStringSubmatch(c.Body, -1) {
			if m[1] == sha {
				h.Attempts++
			}
		}
	}
	return h, nil
}

// RoundsLeft is how many fix attempts the bump may still spend, against
// rounds.deps_fix: never below none.
func (h DepsHistory) RoundsLeft(cap int) int { return max(cap-h.Attempts, 0) }

// Retitle is a Bot PR's title as it lands: a security bump's
// `chore(deps):` becomes `fix(deps):`, so release notes say a vulnerability
// was fixed. Every other title, and the rest of this one, is kept byte for
// byte: Dependabot's text is the only record of what moved.
func Retitle(title string, security bool) string {
	if rest, ok := strings.CutPrefix(title, "chore(deps):"); ok && security {
		return "fix(deps):" + rest
	}
	return title
}

// InjectChecklist is a Bot PR's body with the Bot-PR checklist appended,
// once: a body that carries the marker already comes back unchanged, so
// ticked boxes survive. checklist is the Target Project's
// bot-pr-checklist.md: a file that starts with the marker is injected up to
// its end marker, and any other is wrapped whole. A checklist with no
// unticked item under a verification heading is refused: a round over no
// item would pass a bump nobody looked at.
func InjectChecklist(body, file string) (string, error) {
	unit := checklistUnit(file)
	if len(checklist.Parse(unit)) == 0 {
		return "", errors.New("the Bot-PR checklist holds no unchecked `- [ ]` item under a `## Manual verification` heading: Tier B would have nothing to verify")
	}
	if strings.Contains(body, ChecklistMarker) {
		return body, nil
	}
	if body == "" {
		return unit + "\n", nil
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + "\n" + unit + "\n", nil
}

// UntickChecklist is a Bot PR's body with every box of its Bot-PR checklist
// unticked, and nothing else changed: a box ticked on an older head vouches
// for code that would not merge, so Tier B proves every item on each head.
func UntickChecklist(body string) string {
	at := strings.Index(body, ChecklistMarker)
	if at < 0 {
		return body
	}
	end := len(body)
	if e := strings.Index(body[at:], ChecklistEnd); e >= 0 {
		end = at + e
	}
	block := body[at:end]
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		rest := line[indent:]
		if strings.HasPrefix(rest, "- [x] ") || strings.HasPrefix(rest, "- [X] ") {
			lines[i] = line[:indent] + "- [ ] " + rest[len("- [x] "):]
		}
	}
	return body[:at] + strings.Join(lines, "\n") + body[end:]
}

// checklistUnit is what of the checklist file goes into a body, with no
// trailing newline.
func checklistUnit(file string) string {
	if first, _, _ := strings.Cut(file, "\n"); first == ChecklistMarker {
		if at := strings.Index(file, ChecklistEnd); at >= 0 {
			line := strings.Index(file[at:], "\n")
			if line < 0 {
				return file
			}
			return file[:at+line]
		}
		return strings.TrimSuffix(file, "\n")
	}
	return ChecklistMarker + "\n\n" + strings.TrimSuffix(file, "\n") + "\n\n" + ChecklistEnd
}

// CommitTrailer is a commit message as the Daemon commits it to a Bot PR's
// branch: ending in [dependabot skip], once. The blank tail is dropped and
// the message ends in one newline; a last line that carries the trailer
// anywhere already is left as it is, its trailing space trimmed. An empty
// message is refused: a commit that says only the trailer says nothing of
// the fix.
func CommitTrailer(message string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	last := -1
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			last = i
		}
	}
	if last < 0 {
		return "", errors.New("a commit to a Bot PR needs a message")
	}
	lines = lines[:last+1]
	tail := strings.TrimRight(lines[last], " \t")
	if !strings.Contains(tail, SkipTrailer) {
		tail += " " + SkipTrailer
	}
	lines[last] = tail
	return strings.Join(lines, "\n") + "\n", nil
}

// DepsParkedComment is the one comment that parks a Bot PR at sha whose
// fix budget is spent, naming the cap and the last failure.
func DepsParkedComment(cap int, failure, sha string) string {
	return fmt.Sprintf("The fix budget of %d fix attempts on this bump is spent. Marked draft and labelled `%s`. Last failure: %s. Human triage required.\n\n%s",
		cap, LabelDepsFailed, given(failure), ParkMarker(sha))
}

// DepsConflictParkedComment is the one comment that parks a Bot PR at sha
// whose conflict is a human's to resolve: no fix attempt was spent on it.
func DepsConflictParkedComment(failure, sha string) string {
	return fmt.Sprintf("This bump's conflict cannot be resolved by the Daemon. Marked draft and labelled `%s`. Why: %s. Human triage required.\n\n%s",
		LabelDepsFailed, given(failure), ParkMarker(sha))
}

// given is a failure as a park comment says it.
func given(failure string) string {
	if strings.TrimSpace(failure) == "" {
		return "<not given>"
	}
	return failure
}

// RefusedWarning says a Bot PR is left alone because the deps gate refused
// its head for a reason a human answers.
func RefusedWarning(pr int, reason string) string {
	return fmt.Sprintf("Bot PR #%d is not taken: the deps gate refused its head (%s), and it waits on a human or a new head", pr, reason)
}

// NotTakenWarning says a Bot PR is left alone because the Bot PR Lane is
// off, and why.
func NotTakenWarning(pr int, off string) string {
	return fmt.Sprintf("Bot PR #%d is not taken: the Bot PR Lane is off (%s)", pr, off)
}

// Reviewed is what the human reviews of a Bot PR say of one head. A
// reviewer's last review that approves, requests changes or is dismissed
// stands; a comment changes nothing.
type Reviewed struct {
	// Approved: a standing review approves, and it was left on the head.
	// An approval of an older head vouches for code that would not merge.
	Approved bool
	// ChangesRequested: a standing review requests changes, on any head:
	// a rejection stands until its reviewer takes it back.
	ChangesRequested bool
}

// ReviewOf is what reviews say of sha. The Daemon's own reviews and
// Dependabot's are not a human's, and count for nothing.
func ReviewOf(reviews []github.Review, login, sha string) Reviewed {
	type standing struct{ state, commit string }
	last := map[string]standing{}
	for _, r := range reviews {
		who := strings.ToLower(r.Author)
		if who == "" || strings.EqualFold(who, login) || strings.EqualFold(who, DependabotLogin) {
			continue
		}
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			last[who] = standing{r.State, r.CommitID}
		}
	}
	var out Reviewed
	for _, s := range last {
		out.ChangesRequested = out.ChangesRequested || s.state == "CHANGES_REQUESTED"
		out.Approved = out.Approved || (s.state == "APPROVED" && s.commit == sha)
	}
	out.Approved = out.Approved && !out.ChangesRequested
	return out
}

// The `from A to B` a Dependabot commit message says of each dependency
// it moves.
var versionMove = regexp.MustCompile(`from ([0-9][0-9A-Za-z.+_-]*) to ([0-9][0-9A-Za-z.+_-]*)`)

// MajorBump says a Bot PR is a major bump, from Dependabot's first commit
// message: any dependency whose leading version number goes up, or, below
// 1.0.0, whose minor does, which is where semver puts a breaking change
// there. A message with no move it can read is a major bump too: a bump
// nobody could size is never landed without a human.
func MajorBump(message string) bool {
	moves := versionMove.FindAllStringSubmatch(message, -1)
	if len(moves) == 0 {
		return true
	}
	for _, m := range moves {
		if majorMove(m[1], m[2]) {
			return true
		}
	}
	return false
}

// majorMove says the move from one version to another is major; one it
// cannot read is.
func majorMove(from, to string) bool {
	fMajor, fMinor, okF := leading(from)
	tMajor, tMinor, okT := leading(to)
	switch {
	case !okF || !okT:
		return true
	case tMajor > fMajor:
		return true
	case tMajor == fMajor && fMajor == 0:
		return fMinor < 0 || tMinor < 0 || tMinor > fMinor
	}
	return false
}

// leading are a version's major and minor numbers; minor is -1 when it has
// none that reads as a number. ok is false when its major does not.
func leading(version string) (major, minor int, ok bool) {
	parts := strings.SplitN(version, ".", 3)
	major, ok = number(parts[0])
	minor = -1
	if len(parts) > 1 {
		if n, read := number(parts[1]); read {
			minor = n
		}
	}
	return major, minor, ok
}

// number is the digits a version part starts with.
func number(part string) (int, bool) {
	n, digits := 0, 0
	for _, r := range part {
		if r < '0' || r > '9' {
			break
		}
		n, digits = n*10+int(r-'0'), digits+1
	}
	return n, digits > 0
}

// HandOffComment is the one comment that hands a major bump at sha to a
// human: what moved, both tiers' verdicts, where the tour is, and how to
// answer.
func HandOffComment(summary, tierB, sha string) string {
	return fmt.Sprintf("This is a major bump, so the Daemon will not merge it on its own evidence.\n\n"+
		"- **Bump:** %s\n- **Tier A:** CI green on %s\n- **Tier B:** %s\n- **Screenshot tour:** in this PR's description, under `## Screenshots`, when the Target Project declares a UI Surface\n\n"+
		"Approve this PR (GitHub review) to let the daemon land it; close to reject.\n\n%s",
		given(summary), sha, given(tierB), HandOffMarker(sha))
}

// RefusedComment is the one comment that says the deps gate refused sha for
// a reason a human answers, and what takes the PR back.
func RefusedComment(reason, detail, sha string) string {
	return fmt.Sprintf("The deps gate refused this bump at %s: %s (%s). The Daemon leaves this head alone; a new head (a push, or `@dependabot rebase`) has it taken again.\n\n%s",
		sha, reason, given(detail), RefusedMarker(reason, sha))
}

// DepsBootstrapComment is the one comment that parks a Bot PR at sha whose
// Target Project declares no hermetic tier: Tier B cannot run.
func DepsBootstrapComment(sha string) string {
	return fmt.Sprintf("The Target Project declares no hermetic tier, so Tier B cannot run on this bump. Marked draft and labelled `%s` for a human. Mark the PR ready for review once a hermetic tier is declared to hand it back to the Daemon.\n\n%s",
		LabelVerifyHuman, ParkMarker(sha))
}
