// Package journal writes the Fire record: the JSON the Daemon keeps in the
// State dir for every Fire, with the Steps it ran. It is a log for the
// Dashboard and for people; the Daemon never reads it back to decide
// anything (ADR 0012).
package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/benjr70/auto-agent/internal/budget"
	"github.com/benjr70/auto-agent/internal/step"
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
	// ResetAt is when the Budget gate said to wait until; empty when it
	// let the Fire go, or did not know.
	ResetAt string `json:"resetAt"`
	// Steps are the Steps the Fire ran, in order.
	Steps []Step `json:"steps"`
	// Warnings are things that did not stop the Fire.
	Warnings []string `json:"warnings"`
	// Error is why the Fire failed; empty when it did not.
	Error string `json:"error"`
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
	SessionID string  `json:"sessionId"`
	Model     string  `json:"model"`
	NumTurns  int     `json:"numTurns"`
	CostUSD   float64 `json:"costUsd"`
	// Usage is the call's usage object, verbatim; null when it had none.
	Usage json.RawMessage `json:"usage"`
	// RateLimits are the rate-limit events of the stream, verbatim.
	RateLimits        []json.RawMessage `json:"rateLimits"`
	PermissionDenials int               `json:"permissionDenials"`
	ExitCode          int               `json:"exitCode"`
	ElapsedMS         int64             `json:"elapsedMs"`
}

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
