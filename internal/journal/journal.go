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
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/step"
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
	// Issue is the ticket the Fire worked; null when it worked none.
	Issue *int `json:"issue"`
	// Branch, Commit and PR are what the Fire left on GitHub.
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	PR     *int   `json:"pr"`
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

// NewFireID names a Fire by the second it started and the process that ran
// it, so two Fires never share a record.
func NewFireID(started time.Time) string {
	return started.UTC().Format("20060102T150405Z") + "-" + strconv.Itoa(os.Getpid())
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
