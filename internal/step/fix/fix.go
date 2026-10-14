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
// verification item. This build defines the first; each of the others comes
// with the Lane that needs it, as a kind here and its fields on Brief.
const (
	KindChangeRequest = "change-request"
)

// Brief is what a Fix answers: its Kind, and the fields of that kind.
type Brief struct {
	Kind string `json:"kind"`
	// ChangeRequests are the asks of a pre-commit review (change-request).
	ChangeRequests []string `json:"change_requests,omitempty"`
}

// ChangeRequest is the brief that answers a pre-commit review's asks.
func ChangeRequest(asks []string) Brief {
	return Brief{Kind: KindChangeRequest, ChangeRequests: asks}
}

// Check reports a brief that is not one a Fix can answer.
func (b Brief) Check() error {
	switch b.Kind {
	case KindChangeRequest:
		if len(b.ChangeRequests) == 0 {
			return errors.New("a change-request brief asks for nothing")
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
	// CommitScopes are the scopes a commit subject may use.
	CommitScopes []string `json:"commit_scopes"`
	Brief        Brief    `json:"brief"`
}

// Result is what the Step answers.
type Result struct {
	// Summary says what changed for each item of the brief.
	Summary string `json:"summary"`
	// CommitMessage is a conventional-commit subject and an optional body.
	CommitMessage string `json:"commit_message"`
}

// Check re-checks what the schema cannot express. scopes are the Target
// Project's commit scopes.
func (r Result) Check(scopes []string) error {
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("the summary is empty")
	}
	return step.CheckSubject(step.Subject(r.CommitMessage), scopes)
}
