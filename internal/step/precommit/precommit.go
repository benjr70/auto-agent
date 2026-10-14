// Package precommit is the Pre-commit review Step: before the pull request
// opens, it reads the branch's diff against the ticket and approves it or
// asks for specific changes. It reads and runs the checks; it cannot change
// anything, and a Fix Step makes the changes it asks for.
package precommit

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

// timeout is the wall-clock limit of one review call.
const timeout = 30 * time.Minute

// Name is the Step's name.
const Name = "pre-commit-review"

// The verdicts.
const (
	Approved      = "approved"
	ChangeRequest = "change-request"
)

// readOnlyGit are the git commands the Step may run: they read the history
// and the tree, and change neither.
var readOnlyGit = []string{"Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)"}

// Spec is the Pre-commit review Step for a Target Project with these
// commands. It sees no tool that edits, and its shell may run read-only git
// and exactly the Harness config's test and lint commands.
func Spec(commands implement.Commands) step.Spec {
	allowed := append([]string{"Read", "Glob", "Grep"}, readOnlyGit...)
	for _, command := range []string{commands.Test, commands.Lint} {
		if command = strings.TrimSpace(command); command != "" {
			allowed = append(allowed, "Bash("+command+")")
		}
	}
	return step.Spec{Name: Name, Prompt: prompt, Schema: schema, Tools: []string{"Read", "Glob", "Grep", "Bash"}, Allowed: allowed, Timeout: timeout}
}

// Input is what the Step is told.
type Input struct {
	Issue implement.Issue `json:"issue"`
	// Branch is the branch the checkout is on; Base is the ref it is
	// reviewed against.
	Branch   string             `json:"branch"`
	Base     string             `json:"base"`
	Commands implement.Commands `json:"commands"`
	// CommitScopes are the scopes a commit subject may use.
	CommitScopes []string `json:"commit_scopes"`
	// Round is this review's round, from one; Cap is the last round there
	// is.
	Round int `json:"round"`
	Cap   int `json:"cap"`
}

// Result is what the Step answers.
type Result struct {
	Verdict string `json:"verdict"`
	// ChangeRequests are the specific changes asked for, one each: empty on
	// approval.
	ChangeRequests []string `json:"change_requests"`
}

// Approved reports whether the review approved the branch.
func (r Result) Approved() bool { return r.Verdict == Approved }

// Check re-checks what the schema cannot express: an approval asks for
// nothing, and a change request asks for something.
func (r Result) Check() error {
	switch {
	case r.Verdict == Approved && len(r.ChangeRequests) > 0:
		return errors.New("an approval carries change requests")
	case r.Verdict == ChangeRequest && len(r.ChangeRequests) == 0:
		return errors.New("a change request asks for nothing")
	}
	for i, ask := range r.ChangeRequests {
		if strings.TrimSpace(ask) == "" {
			return fmt.Errorf("change request %d is empty", i+1)
		}
	}
	return nil
}
