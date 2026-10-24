// Package journal writes the Fire record: the JSON the Daemon keeps in the
// State dir for every Fire, with the Steps it ran. It is a log for the
// Dashboard and for people. The Daemon decides nothing from it: the one
// thing it reads back is a session id, to resume a Step instead of running
// it again (ADR 0012).
package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/ci"
	"github.com/benjr70/auto-agent/internal/docsgate"
	"github.com/benjr70/auto-agent/internal/finding"
	"github.com/benjr70/auto-agent/internal/step"
	"github.com/benjr70/auto-agent/internal/step/arbitrate"
	"github.com/benjr70/auto-agent/internal/verify"
)

// dirName is the directory of the State dir the records live in.
const dirName = "fires"

// Journal is the Fire records of one State dir.
type Journal struct {
	StateDir string
}

// Record is one Fire.
type Record struct {
	FireID string `json:"fireId"`
	// Repo is the Target Project, as owner/name.
	Repo      string    `json:"repo"`
	StartedAt time.Time `json:"startedAt"`
	// EndedAt is null while the Fire is in flight.
	EndedAt *time.Time `json:"endedAt"`
	// Exit is the Fire's exit code; null while it is in flight.
	Exit *int `json:"exit"`
	// Phase is how far the Fire got: the last thing it started.
	Phase string `json:"phase"`
	// Kind is the state the Fire derived; empty when it never got that far.
	Kind string `json:"kind"`
	// Action is what the Fire set out to do from the state it derived last;
	// empty when it never got that far, "none" when there was nothing.
	Action string `json:"action"`
	// Issue is the ticket the Fire worked; null when it worked none.
	Issue *int `json:"issue"`
	// Branch, Commit and PR are what the Fire left on GitHub; PR is also
	// the Agent PR the reconcile Lane took.
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	PR     *int   `json:"pr"`
	// Reconcile is the reason the reconcile Lane took PR; empty when it
	// took none.
	Reconcile string `json:"reconcile"`
	// NotWorked says what the Fire found to do that this build does not do
	// yet; empty when there was no such thing.
	NotWorked string `json:"notWorked"`
	// ResumeSession is the session the ticket's next Fire takes up again,
	// when this one left its Step cut off; empty when there is none.
	ResumeSession string `json:"resumeSession"`
	// ResetAt is when the Budget gate said to wait until; empty when it
	// let the Fire go, or did not know.
	ResetAt string `json:"resetAt"`
	// ReviewRounds is how many rounds of pre-commit review the Fire ran,
	// failed smokes counted with them against the cap.
	ReviewRounds int `json:"reviewRounds"`
	// Smoke is how the smoke went; null when it did not run.
	Smoke *verify.Result `json:"smoke"`
	// Review is what the one-time review of the PR found; null when it did
	// not run.
	Review *Review `json:"review"`
	// Revise is how the PR's review threads were answered; null when they
	// were not worked.
	Revise *Revise `json:"revise"`
	// CI is the Fire's last wait on a PR's checks; null when it waited on
	// none.
	CI *CI `json:"ci"`
	// Rebase is the Fire's rebase of the PR's branch; null when it made
	// none.
	Rebase *Rebase `json:"rebase"`
	// ParkedPR is the park label the Fire left on the PR for a human; empty
	// when it parked none.
	ParkedPR string `json:"parkedPR"`
	// CIRounds is how many rounds of CI fixes the Fire ran, against
	// rounds.pr_watch.
	CIRounds int `json:"ciRounds"`
	// Verify is the Verification round the Fire ran; null when it ran
	// none.
	Verify *Verify `json:"verify"`
	// Ruling is the Ruling request the Fire posted; null when it posted
	// none.
	Ruling *Ruling `json:"ruling"`
	// Applied is how the Fire answered the human's reply to a Ruling
	// request; null when it answered none.
	Applied *RulingApplied `json:"applied"`
	// Resolve is the Decision ticket the Fire worked; null when it worked
	// none.
	Resolve *Resolve `json:"resolve"`
	// Deps is the Bot PR the Fire took through the Bot PR Lane; null when
	// it took none.
	Deps *Deps `json:"deps"`
	// Park is the Parked state the Fire left the Daemon in; null when it
	// is not Parked.
	Park *Park `json:"park"`
	// Steps are the Steps the Fire ran, in order.
	Steps []Step `json:"steps"`
	// Warnings are things that did not stop the Fire.
	Warnings []string `json:"warnings"`
	// Error is why the Fire failed; empty when it did not.
	Error string `json:"error"`
}

// Resolve is a Fire's work on a Decision ticket.
type Resolve struct {
	// Type is the ticket's wayfinder type: research or task.
	Type string `json:"type"`
	// Slug is the ticket's slug, which names the branch research/<slug>.
	Slug string `json:"slug"`
	// Findings is the findings file's path.
	Findings string `json:"findings"`
	// PR is the research PR the Fire opened; null when it opened none.
	PR *int `json:"pr"`
	// Failed is how the ticket failed, when the Fire left it AFK:failed:
	// no-sources, research-error once its attempts ran out (task-error for
	// a task), needs-human,
	// pr-watch-exhausted, or gate-refused; empty when it did not.
	Failed string `json:"failed"`
	// Gate is the docs-only gate's verdict on the research PR; null when
	// the gate did not run.
	Gate *docsgate.Verdict `json:"gate"`
	// Merged is the commit the research PR landed as; empty when it did
	// not land.
	Merged string `json:"merged"`
	// Resolved: the ticket was commented on, closed and AFK:done.
	Resolved bool `json:"resolved"`
	// HITL: a task that needs product code, relabelled HITL and taken off
	// the pick signal.
	HITL bool `json:"hitl"`
	// Graduated are the tickets the Graduate fog Step's proposals became,
	// in the order proposed; empty when it created none.
	Graduated []Graduated `json:"graduated"`
}

// Graduated is one ticket fog graduation created.
type Graduated struct {
	Issue int    `json:"issue"`
	Type  string `json:"type"`
	// Depth is the length of its Spawned by chain, as far as it was
	// followed: 3 is any depth past the deepest an AFK ticket may be.
	Depth int `json:"depth"`
	// Route is AFK or HITL, as Go decided it.
	Route string `json:"route"`
	// Wired: it is the Map's sub-issue, and every blocked_by edge it was
	// proposed with is on it.
	Wired bool `json:"wired"`
	// Picked: an AFK ticket that went on the pick signal.
	Picked bool `json:"picked"`
}

// Review is what the one-time review of an Agent PR found and posted.
type Review struct {
	// Reviewed is the commit the review read.
	Reviewed string `json:"reviewed"`
	// Findings counts every Finding, all three kinds; Threads the defect
	// threads the PR has; Notes the Review notes.
	Findings int `json:"findings"`
	Threads  int `json:"threads"`
	Notes    int `json:"notes"`
	// Ambiguities are the product ambiguities, for the Ruling request.
	Ambiguities []finding.Finding `json:"ambiguities"`
	// ConfigChanged are the paths under the Harness config directory the PR
	// changes, which a human must review.
	ConfigChanged []string `json:"configChanged"`
}

// Revise is how a Fire answered an Agent PR's review threads.
type Revise struct {
	// Rounds is how many Fix rounds it ran, against rounds.revise.
	Rounds int `json:"rounds"`
	// Fixed are the threads it fixed, or answered no-change on the human's
	// ruling, and resolved.
	Fixed []string `json:"fixed"`
	// Disputes are the threads it disputed and left open: a bot-authored thread's
	// for the Arbiter, a human's for the Ruling request.
	Disputes []Dispute `json:"disputes"`
	// Waiting are the Disputes open on the PR, as last read, that nobody
	// has answered yet: this Fire's, and any an earlier Fire left.
	Waiting []Dispute `json:"waiting"`
	// Asked are the threads the PR's open Ruling request already asks the
	// human about: the Arbiter leaves them alone.
	Asked []string `json:"asked"`
	// Arbiter are the Arbiter's verdicts on the Disputes, as Go read them;
	// empty when it did not run.
	Arbiter []arbitrate.Verdict `json:"arbiter"`
	// Dismissed are the threads the Arbiter dismissed, and Go resolved.
	Dismissed []string `json:"dismissed"`
	// Decisions are what only the human can settle, for the Ruling request:
	// the Arbiter's ambiguities, the threads it left unruled, and its fix
	// verdicts the Fix could not apply.
	Decisions []arbitrate.Verdict `json:"decisions"`
	// Failing are the threads still open at the round cap, when the PR
	// was parked with AFK:revise-failed.
	Failing []string `json:"failing"`
}

// Dispute is one thread a Fix disputed.
type Dispute struct {
	Thread string `json:"thread"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
	// Authored is "bot" or "human": who opened the thread, which decides
	// who answers the Dispute.
	Authored string `json:"authored"`
	// Reply is why, as posted in the thread.
	Reply string `json:"reply"`
}

// Rebase is a rebase of a PR's branch onto the default branch.
type Rebase struct {
	// Onto is the ref it rebased onto.
	Onto string `json:"onto"`
	// Stops counts the conflicts a Fix resolved; Conflicts are their files.
	Stops     int      `json:"stops"`
	Conflicts []string `json:"conflicts"`
	// Failed says why it was given up; empty when it was pushed.
	Failed string `json:"failed"`
}

// The outcomes of a Fire in the Bot PR Lane.
const (
	// DepsTierAGreen: Tier A is green on a head its Fixes pushed, and its
	// marker is on the PR; the next Fire takes that head to Tier B.
	DepsTierAGreen = "tier-a-green"
	// DepsTierBFixed: a Fix answered a failed Tier B and was pushed; the
	// next Fire proves the new head.
	DepsTierBFixed = "tier-b-fixed"
	// DepsTierBInfra: Tier B's environment did not boot. No attempt is
	// spent; the next Fire runs the round again.
	DepsTierBInfra = "tier-b-infra"
	// DepsVerifyHuman: the Target Project declares no hermetic tier, so
	// Tier B cannot run: drafted and labelled AFK:verify-human.
	DepsVerifyHuman = "verify-human"
	// DepsMerged: the deps gate approved the head, and it was merged.
	DepsMerged = "merged"
	// DepsHITL: a major bump both tiers passed, handed to a human with HITL.
	DepsHITL = "hitl"
	// DepsRefused: the deps gate refused, or the merge it approved did not
	// land; Gate says why.
	DepsRefused = "refused"
	// DepsNudged: a conflict on Dependabot's own commits, answered by asking
	// Dependabot to rebase.
	DepsNudged = "nudged"
	// DepsRebased: a conflict on a branch the Daemon pushed to, rebased and
	// pushed with a lease.
	DepsRebased = "rebased"
	// DepsWaiting: CI has not settled, or ran no check, at Tier A or at the
	// deps gate; no attempt is spent, and the next Fire reads it again.
	DepsWaiting = "waiting-ci"
	// DepsParked: drafted and labelled AFK:deps-failed for a human.
	DepsParked = "parked"
	// DepsSuperseded: the PR closed, became a draft, or moved under the
	// Fire. Nothing is done to it.
	DepsSuperseded = "superseded"
)

// Deps is a Fire's work on a Bot PR.
type Deps struct {
	// Head is the head sha the Fire found.
	Head     string `json:"head"`
	Security bool   `json:"security"`
	// Retitled and Checklist are set when this Fire retitled the PR and
	// injected its Bot-PR checklist.
	Retitled  bool `json:"retitled"`
	Checklist bool `json:"checklist"`
	// TierA is where Tier A stands: green, fixed(k) after k Fixes this Fire,
	// failed, pending or skipped.
	TierA string `json:"tierA"`
	// TierB is where Tier B stands: k/n items passed, with " failed" when
	// the PR was parked on it, infra, or skipped when its marker was on the
	// head or it did not run; empty when the Fire ended before it.
	TierB string `json:"tierB"`
	// Major: the deps gate sized the bump major.
	Major bool `json:"major"`
	// Gate is the deps gate's verdict; empty when it was not asked.
	Gate string `json:"gate"`
	// Merged is the commit the bump landed as; empty when it was not
	// merged.
	Merged string `json:"merged"`
	// Attempts are the fix attempts spent on the bump, against Cap,
	// rounds.deps_fix: the budget of both tiers together.
	Attempts int    `json:"attempts"`
	Cap      int    `json:"cap"`
	Outcome  string `json:"outcome"`
}

// Verify is one Verification round of an Agent PR.
type Verify struct {
	// Round and Max are the round and rounds.manual_verify.
	Round int `json:"round"`
	Max   int `json:"max"`
	// Line is the round's `manual-verify:` line.
	Line string `json:"line"`
	// Dir is the round's evidence directory; empty when nothing booted.
	Dir string `json:"dir"`
	// Bootstrap: the head declares no hermetic tier, and a human verifies
	// the PR.
	Bootstrap bool `json:"bootstrap"`
	// Infra says why the round gave no verdict; empty when it gave them.
	Infra string `json:"infra"`
	// Shots is how many screenshots the PR's description carries from it.
	Shots int `json:"shots"`
	// Done: every item passed or was justifiably deferred, and the ticket
	// is AFK:done.
	Done bool `json:"done"`
	// FixRounds counts the Fixes this Fire ran on failed items before this
	// round.
	FixRounds int `json:"fixRounds"`
	// Exhausted: the round still had failed items at rounds.manual_verify,
	// and the PR is parked.
	Exhausted bool `json:"exhausted"`
	// Dispute is why the Fix said an acceptance criterion is itself wrong;
	// the PR is parked. Empty when no Fix disputed.
	Dispute string `json:"dispute"`
}

// CI is a wait on the checks of a PR's head.
type CI struct {
	Head string `json:"head"`
	// Result is green, fail, none (no check ran), timeout or error.
	Result ci.Result `json:"result"`
	// Reads counts the reads of the checks it took.
	Reads  int           `json:"reads"`
	Failed []FailedCheck `json:"failed"`
}

// FailedCheck is one check that failed, with the end of its log.
type FailedCheck struct {
	Name string `json:"name"`
	Link string `json:"link"`
	Log  string `json:"log"`
}

// Ruling is a Ruling request a Fire posted.
type Ruling struct {
	// Head is the commit it asks about.
	Head string `json:"head"`
	// Decisions counts its decisions; New those this Fire added.
	Decisions int `json:"decisions"`
	New       int `json:"new"`
	// Supersedes is the earlier request it replaces; 0 when none.
	Supersedes int64 `json:"supersedes,omitempty"`
	// Written says the Write Ruling request Step wrote the decisions; false
	// when the Arbiter's drafts stood as they were.
	Written bool `json:"written"`
}

// RulingApplied is how a Fire answered the human's reply to a Ruling request.
type RulingApplied struct {
	// Ruling is the reply's Ruling, as "1A 2B"; Status says whether it is
	// full, partial or invalid.
	Ruling string `json:"ruling"`
	Status string `json:"status"`
	// Nudged says this Fire posted the one nudge a reply that is not a full
	// Ruling gets.
	Nudged bool `json:"nudged"`
	// Fixed are the decisions whose chosen option changed code.
	Fixed []int `json:"fixed"`
}

// Park is the Daemon's Parked state, as a Fire left it.
type Park struct {
	// Issue is the AFK:needs-human issue; null when it could not be opened.
	Issue *int `json:"issue"`
	// Reason is why the credential counts as dead.
	Reason string `json:"reason"`
	// ProbeAt is when the Parked Daemon probes the credential next.
	ProbeAt time.Time `json:"probeAt"`
}

// Step is one Step of a Fire.
type Step struct {
	Name string `json:"name"`
	// Gate is the Gate verdict the Step was asked on.
	Gate      *budget.Verdict `json:"gate"`
	StartedAt time.Time       `json:"startedAt"`
	// Outcome is how the call ended; empty when the Budget gate stopped
	// the Step before it ran.
	Outcome step.Outcome `json:"outcome"`
	// Detail says why, for every Outcome but OK.
	Detail string `json:"detail"`
	// SessionID lets a cut-off Step be resumed.
	SessionID string `json:"sessionId"`
	// Resume says why the call took up an earlier session; empty when it
	// started its own.
	Resume   Resume  `json:"resume"`
	Model    string  `json:"model"`
	NumTurns int     `json:"numTurns"`
	CostUSD  float64 `json:"costUsd"`
	// Usage is the call's usage object, verbatim; null when it had none.
	Usage json.RawMessage `json:"usage"`
	// RateLimits are the rate-limit events of the stream, verbatim.
	RateLimits        []json.RawMessage `json:"rateLimits"`
	PermissionDenials int               `json:"permissionDenials"`
	ExitCode          int               `json:"exitCode"`
	ElapsedMS         int64             `json:"elapsedMs"`
}

// Resume is why a call took up an earlier session.
type Resume string

const (
	// ResumePaused: the usage limit cut the Step off in an earlier Fire.
	ResumePaused Resume = "paused"
	// ResumeCorrection: the Step's result was not accepted, and it is given
	// one chance to correct it.
	ResumeCorrection Resume = "correction"
)

// NotRun is the journal's entry for a Step the Budget gate stopped.
func NotRun(name string, at time.Time, gate budget.Verdict) Step {
	return StepOf(name, at, gate, step.Run{})
}

// StepOf is the journal's entry for one classified call.
func StepOf(name string, started time.Time, gate budget.Verdict, run step.Run) Step {
	entry := Step{
		Name: name, Gate: &gate, StartedAt: started.UTC(), Outcome: run.Outcome, Detail: run.Detail,
		SessionID: run.SessionID, Model: run.Model, NumTurns: run.NumTurns, CostUSD: run.CostUSD,
		Usage: run.RawUsage, RateLimits: run.RateLimits, PermissionDenials: len(run.PermissionDenials),
		ExitCode: run.ExitCode, ElapsedMS: run.Elapsed.Milliseconds(),
	}
	if len(entry.Usage) == 0 {
		entry.Usage = json.RawMessage("null")
	}
	if entry.RateLimits == nil {
		entry.RateLimits = []json.RawMessage{}
	}
	return entry
}

// lastID is the second and the count of the Fire IDs this process gave
// out last.
var lastID struct {
	sync.Mutex
	second string
	count  int
}

// NewFireID names a Fire by the second it started and the process that ran
// it, so two Fires never share a record. A Daemon that starts a second Fire
// within the same second numbers it: `_2` sorts after the first.
func NewFireID(started time.Time) string {
	id := started.UTC().Format("20060102T150405Z") + "-" + strconv.Itoa(os.Getpid())
	lastID.Lock()
	defer lastID.Unlock()
	if lastID.second != id {
		lastID.second, lastID.count = id, 1
		return id
	}
	lastID.count++
	return id + "_" + strconv.Itoa(lastID.count)
}

// Path is where the record of a Fire is kept.
func (j Journal) Path(fireID string) string {
	return filepath.Join(j.StateDir, dirName, fireID+".json")
}

// Write stores the record, replacing an earlier write of the same Fire. A
// reader never sees half a record: it is written aside and moved into place.
func (j Journal) Write(record Record) error {
	if record.Steps == nil {
		record.Steps = []Step{}
	}
	if record.Warnings == nil {
		record.Warnings = []string{}
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	path := j.Path(record.FireID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Session is the session to resume a paused ticket's Step in: the one the
// newest Fire that ran a call for the ticket left to be resumed, when that
// call was this Step's. It is empty when there is none, whatever the
// reason: the Step is then run again.
func (j Journal) Session(repo string, issue int, stepName string) string {
	paths, _ := filepath.Glob(filepath.Join(j.StateDir, dirName, "*.json"))
	// A Fire id starts with the second the Fire started.
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record struct {
			Repo          string `json:"repo"`
			Issue         *int   `json:"issue"`
			ResumeSession string `json:"resumeSession"`
			Steps         []struct {
				Name    string       `json:"name"`
				Outcome step.Outcome `json:"outcome"`
			} `json:"steps"`
		}
		if json.Unmarshal(data, &record) != nil || record.Repo != repo || record.Issue == nil || *record.Issue != issue {
			continue
		}
		for i := len(record.Steps) - 1; i >= 0; i-- {
			// A Step the Budget gate stopped ran no call, and neither did a
			// Fire that died before its first: an older Fire decides.
			if last := record.Steps[i]; last.Outcome != "" {
				if last.Name == stepName {
					return record.ResumeSession
				}
				return ""
			}
		}
	}
	return ""
}
