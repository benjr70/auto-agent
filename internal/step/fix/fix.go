// Package fix is the Fix Step: it changes a ticket's branch to answer a typed
// brief. The Step changes files and answers; Go commits and pushes.
package fix

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/implement"
)

//go:embed prompt.md
var prompt string

//go:embed schema.json
var schema string

// timeout is the wall-clock limit of one Fix call.
const timeout = time.Hour

// Spec is the Fix Step. Like Implement it gets the tools to read, edit and
// run commands, and no credential.
func Spec() step.Spec {
	tools := []string{"Read", "Edit", "Write", "Glob", "Grep", "Bash"}
	return step.Spec{Name: "fix", Prompt: prompt, Schema: schema, Tools: tools, Allowed: tools, Timeout: timeout}
}

// The kinds of brief. A Fix answers one of five: a pre-commit change
// request, a failing CI run, review threads, a rebase conflict, or a failed
// verification item. This build defines three; each of the others comes with
// the Lane that needs it, as a kind here and its fields on Brief.
const (
	KindChangeRequest      = "change-request"
	KindFailedVerification = "failed-verification"
	KindReviewThreads      = "review-threads"
	KindCIFailure          = "ci-failure"
	KindRebaseConflict     = "rebase-conflict"
)

// Brief is what a Fix answers: its Kind, and the fields of that kind.
type Brief struct {
	Kind string `json:"kind"`
	// ChangeRequests are the asks of a pre-commit review (change-request).
	ChangeRequests []string `json:"change_requests,omitempty"`
	// Failed are the verification items that failed, each with its
	// evidence (failed-verification).
	Failed []Verification `json:"failed,omitempty"`
	// Specs are the deferrals of a Verification round that demand a
	// post-deploy check the PR does not carry yet (failed-verification).
	Specs []DemandedSpec `json:"specs,omitempty"`
	// Threads are the PR's open review threads, each whole
	// (review-threads).
	Threads []Thread `json:"threads,omitempty"`
	// CI are the checks that failed on the PR's head, each with the end of
	// its log (ci-failure).
	CI []FailedCheck `json:"ci,omitempty"`
	// Conflicts are the files a rebase onto the base stopped on, each holding
	// git's conflict markers (rebase-conflict).
	Conflicts []string `json:"conflicts,omitempty"`
	// Ruling is what the human chose in their Ruling, for each decision
	// whose chosen option changes code (review-threads).
	Ruling []Ruled `json:"ruling,omitempty"`
}

// FailedCheck is one CI check that failed, and the end of its log.
type FailedCheck struct {
	Name string `json:"name"`
	Link string `json:"link"`
	Log  string `json:"log"`
}

// RebaseConflict is the brief that resolves a rebase's conflicts in place.
func RebaseConflict(files []string) Brief {
	return Brief{Kind: KindRebaseConflict, Conflicts: files}
}

// CIFailure is the brief that answers a failing CI run.
func CIFailure(failed []FailedCheck) Brief {
	return Brief{Kind: KindCIFailure, CI: failed}
}

// Ruled is one decision of a Ruling the Fix applies: what was asked, and
// the option the human chose, which is the instruction.
type Ruled struct {
	// Decision is its number in the Ruling request, from 1.
	Decision int    `json:"decision"`
	Title    string `json:"title"`
	Scenario string `json:"scenario"`
	Now      string `json:"now"`
	// Chosen is the option's text, and Cost what it said it costs.
	Chosen string `json:"chosen"`
	Cost   string `json:"cost"`
	// Path and Line are where its thread is; empty when it came from none.
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
}

// Thread is one open review thread on the PR, whole: where it is, who
// opened it, and every comment in it.
type Thread struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	// Line is 0 when the line has left the diff.
	Line int `json:"line,omitempty"`
	// Authored is "bot", the PR's automated review, or "human".
	Authored string    `json:"authored"`
	Comments []Comment `json:"comments"`
	// Ruling is the human's last reply, when it answers the Daemon's own:
	// the thread is decided, and may not be disputed.
	Ruling string `json:"ruling,omitempty"`
	// ArbiterFix is the instruction of the Arbiter's fix verdict on a
	// disputed thread: binding. The Fix applies it; a Dispute of it says it
	// cannot be applied, and takes it to the human.
	ArbiterFix string `json:"arbiter_fix,omitempty"`
}

// Comment is one comment of a thread, and whose voice it is in.
type Comment struct {
	// From is "review", the automated review; "daemon", the Daemon's own
	// reply; or "human".
	From string `json:"from"`
	Body string `json:"body"`
}

// The voices of a thread's comments.
const (
	FromReview = "review"
	FromDaemon = "daemon"
	FromHuman  = "human"
)

// Verification is one verification item that failed: what was checked, what
// it said, and the end of its output.
type Verification struct {
	Item   string `json:"item"`
	Detail string `json:"detail"`
	Log    string `json:"log,omitempty"`
}

// DemandedSpec is one item a Verification round deferred to a deployed
// environment, and the post-deploy check it demanded for it. ID is its place
// in the brief's specs, from 1: what the Fix answers it by.
type DemandedSpec struct {
	ID   int    `json:"id"`
	Item string `json:"item"`
	Spec string `json:"spec"`
}

// FailedVerification is the brief that answers failed verification items
// and the post-deploy checks a round demanded.
func FailedVerification(failed []Verification, specs []DemandedSpec) Brief {
	return Brief{Kind: KindFailedVerification, Failed: failed, Specs: specs}
}

// ChangeRequest is the brief that answers a pre-commit review's asks.
func ChangeRequest(asks []string) Brief {
	return Brief{Kind: KindChangeRequest, ChangeRequests: asks}
}

// ReviewThreads is the brief that answers a PR's open review threads.
func ReviewThreads(threads []Thread) Brief {
	return Brief{Kind: KindReviewThreads, Threads: threads}
}

// Ruling is the brief that applies the human's Ruling: the chosen options
// that change code, and nothing else.
func Ruling(ruled []Ruled) Brief {
	return Brief{Kind: KindReviewThreads, Ruling: ruled}
}

// Check reports a brief that is not one a Fix can answer.
func (b Brief) Check() error {
	switch b.Kind {
	case KindChangeRequest:
		if len(b.ChangeRequests) == 0 {
			return errors.New("a change-request brief asks for nothing")
		}
		return nil
	case KindFailedVerification:
		if len(b.Failed) == 0 && len(b.Specs) == 0 {
			return errors.New("a failed-verification brief names no item")
		}
		for _, v := range b.Failed {
			if strings.TrimSpace(v.Item) == "" {
				return errors.New("a failed-verification brief names no item")
			}
		}
		for i, s := range b.Specs {
			if s.ID != i+1 || strings.TrimSpace(s.Item) == "" || strings.TrimSpace(s.Spec) == "" {
				return fmt.Errorf("demanded spec %d is not item %d with its spec", s.ID, i+1)
			}
		}
		return nil
	case KindRebaseConflict:
		if len(b.Conflicts) == 0 {
			return errors.New("a rebase-conflict brief names no conflicted file")
		}
		return nil
	case KindCIFailure:
		if len(b.CI) == 0 {
			return errors.New("a ci-failure brief names no failed check")
		}
		return nil
	case KindReviewThreads:
		if len(b.Threads) == 0 && len(b.Ruling) == 0 {
			return errors.New("a review-threads brief has no thread and no Ruling")
		}
		return nil
	}
	return fmt.Errorf("%q is not a kind of Fix brief", b.Kind)
}

// Input is what the Step is told.
type Input struct {
	Issue implement.Issue `json:"issue"`
	// Branch is the branch the checkout is on.
	Branch   string             `json:"branch"`
	Commands implement.Commands `json:"commands"`
	// Base is the branch the PR merges into, for a brief about a PR.
	Base string `json:"base,omitempty"`
	// CommitScopes are the scopes a commit subject may use.
	CommitScopes []string `json:"commit_scopes"`
	Brief        Brief    `json:"brief"`
	// Bump is the Bot PR the branch is, when it is one: a dependency bump
	// with no ticket.
	Bump *Bump `json:"bump,omitempty"`
}

// Bump is a Bot PR as a Fix on its branch is told it: Dependabot's title
// and body, its release notes, and the Target Project's recipe for its
// lockfile.
type Bump struct {
	PR    int    `json:"pr"`
	Title string `json:"title"`
	Body  string `json:"body"`
	// LockfileRefresh is the Harness config's commands.lockfile_refresh;
	// empty when it names none.
	LockfileRefresh string `json:"lockfile_refresh,omitempty"`
}

// Result is what the Step answers.
type Result struct {
	// Summary says what changed for each item of the brief.
	Summary string `json:"summary"`
	// CommitMessage is a conventional-commit subject and an optional body.
	CommitMessage string `json:"commit_message"`
	// Threads answer a review-threads brief, one per thread; empty for any
	// other kind.
	Threads []Answer `json:"threads"`
	// Flake says why a failing CI run is the infrastructure's, not the
	// code's, when the Fix changed nothing for it; empty otherwise.
	Flake string `json:"flake"`
	// PostDeploy answers a failed-verification brief's specs, one per spec:
	// the check the Daemon tags its item with. Empty for any other kind.
	PostDeploy []PostDeploy `json:"post_deploy"`
	// Dispute says why an acceptance criterion a failed-verification brief
	// holds the product to is itself wrong; the Fix changed nothing. Empty
	// otherwise.
	Dispute string `json:"dispute"`
}

// PostDeploy is the post-deploy check that answers one demanded spec: what
// goes in its item's `<!-- post-deploy: … -->` tag.
type PostDeploy struct {
	Spec  int    `json:"spec"`
	Check string `json:"check"`
}

// Answer is how a Fix answered one review thread.
type Answer struct {
	Thread string `json:"thread"`
	// Action is Fixed, Disputed or NoChange.
	Action string `json:"action"`
	// Reply is what the Daemon posts in the thread, in plain language.
	Reply string `json:"reply"`
}

// The answers to a thread.
const (
	// Fixed: the code was changed as the thread asks.
	Fixed = "fixed"
	// Disputed: the thread is wrong or must not be applied; nothing changed
	// for it.
	Disputed = "disputed"
	// NoChange: the human's ruling on the thread asks for no change.
	NoChange = "no-change"
)

// Check re-checks what the schema cannot express, for the brief it answers.
// scopes are the Target Project's commit scopes. A review-threads brief is
// answered once per thread: a ruled thread is fixed or answered no-change,
// never disputed; any other is fixed or disputed.
func (r Result) Check(brief Brief, scopes []string) error {
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("the summary is empty")
	}
	if err := step.CheckSubject(step.Subject(r.CommitMessage), scopes); err != nil {
		return err
	}
	if strings.TrimSpace(r.Flake) != "" && brief.Kind != KindCIFailure {
		return fmt.Errorf("a flake was named for a %s brief", brief.Kind)
	}
	if brief.Kind == KindFailedVerification {
		return r.checkVerification(brief)
	}
	if strings.TrimSpace(r.Dispute) != "" || len(r.PostDeploy) > 0 {
		return fmt.Errorf("a dispute or a post-deploy check was answered for a %s brief", brief.Kind)
	}
	if brief.Kind != KindReviewThreads {
		if len(r.Threads) > 0 {
			return fmt.Errorf("threads were answered for a %s brief", brief.Kind)
		}
		return nil
	}
	answered := map[string]Answer{}
	for _, a := range r.Threads {
		if _, twice := answered[a.Thread]; twice {
			return fmt.Errorf("thread %s is answered twice", a.Thread)
		}
		answered[a.Thread] = a
	}
	for _, t := range brief.Threads {
		a, ok := answered[t.ID]
		switch {
		case !ok:
			return fmt.Errorf("thread %s is not answered", t.ID)
		case strings.TrimSpace(a.Reply) == "":
			return fmt.Errorf("thread %s has no reply", t.ID)
		case t.Ruling != "" && a.Action == Disputed:
			return fmt.Errorf("thread %s is disputed, but the human ruled it: apply the ruling, or answer %s when it asks for no change", t.ID, NoChange)
		case t.Ruling == "" && a.Action == NoChange:
			return fmt.Errorf("thread %s is answered %s, but only a thread the human ruled may be", t.ID, NoChange)
		case a.Action != Fixed && a.Action != Disputed && a.Action != NoChange:
			return fmt.Errorf("thread %s is answered %q, not %s, %s or %s", t.ID, a.Action, Fixed, Disputed, NoChange)
		}
		delete(answered, t.ID)
	}
	for id := range answered {
		return fmt.Errorf("thread %s is answered, but is not in the brief", id)
	}
	return nil
}

// checkVerification checks the answer to a failed-verification brief: a
// dispute answers nothing else; otherwise every demanded spec is answered
// once, with a check that fits in its item's tag.
func (r Result) checkVerification(brief Brief) error {
	if len(r.Threads) > 0 {
		return fmt.Errorf("threads were answered for a %s brief", brief.Kind)
	}
	if strings.TrimSpace(r.Dispute) != "" {
		if len(r.PostDeploy) > 0 {
			return errors.New("a dispute changes nothing, yet post-deploy checks were answered")
		}
		return nil
	}
	answered := map[int]bool{}
	for _, p := range r.PostDeploy {
		switch check := strings.TrimSpace(p.Check); {
		case p.Spec < 1 || p.Spec > len(brief.Specs):
			return fmt.Errorf("post-deploy check %d answers no spec of the brief", p.Spec)
		case answered[p.Spec]:
			return fmt.Errorf("spec %d is answered twice", p.Spec)
		case check == "" || strings.ContainsAny(check, "\r\n") || strings.Contains(check, "-->"):
			return fmt.Errorf("the check for spec %d is not one line that fits in a post-deploy tag", p.Spec)
		}
		answered[p.Spec] = true
	}
	for _, s := range brief.Specs {
		if !answered[s.ID] {
			return fmt.Errorf("spec %d (%q) is not answered", s.ID, s.Item)
		}
	}
	return nil
}
