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
	// Approved: a human approved it in a review, which a HITL one waits on.
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
	shaShape         = regexp.MustCompile(`^\S+$`)
)

// The tier markers.
func tierAMarker(sha string) string { return fmt.Sprintf("<!-- deps-lane tierA=green sha=%s -->", sha) }
func tierBMarker(sha string) string { return fmt.Sprintf("<!-- deps-lane tierB=PASS sha=%s -->", sha) }

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
	if strings.TrimSpace(failure) == "" {
		failure = "<not given>"
	}
	return fmt.Sprintf("The fix budget of %d fix attempts on this bump is spent. Marked draft and labelled `%s`. Last failure: %s. Human triage required.\n\n%s",
		cap, LabelDepsFailed, failure, ParkMarker(sha))
}

// NotTakenWarning says a Bot PR is left alone because the Bot PR Lane is
// off, and why.
func NotTakenWarning(pr int, off string) string {
	return fmt.Sprintf("Bot PR #%d is not taken: the Bot PR Lane is off (%s)", pr, off)
}
