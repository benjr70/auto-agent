// Package implement is the Implement Step: one ticket, test-first, in the
// Daemon's clone. The Step changes files and answers; Go commits, pushes and
// opens the pull request from the answer.
package implement

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/step"
)

//go:embed prompt.md
var prompt string

//go:embed schema.json
var schema string

// timeout is the wall-clock limit of one Implement call.
const timeout = 2 * time.Hour

// Spec is the Implement Step. It gets the tools to read, edit and run
// commands, the shell included: what keeps it off GitHub is the runner, which
// gives it no credential.
func Spec() step.Spec {
	tools := []string{"Read", "Edit", "Write", "Glob", "Grep", "Bash"}
	return step.Spec{Name: "implement", Prompt: prompt, Schema: schema, Tools: tools, Allowed: tools, Timeout: timeout}
}

// Input is what the Step is told.
type Input struct {
	Issue Issue `json:"issue"`
	// Branch is the branch the checkout is on.
	Branch   string   `json:"branch"`
	Commands Commands `json:"commands"`
	// CommitScopes are the scopes a commit subject may use.
	CommitScopes []string `json:"commit_scopes"`
}

// Issue is the ticket to implement.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// Commands are the Harness config commands the Step runs.
type Commands struct {
	Install string `json:"install"`
	Test    string `json:"test"`
	Lint    string `json:"lint,omitempty"`
}

// Result is what the Step answers.
type Result struct {
	// Finished is whether every acceptance criterion is implemented and the
	// tests pass.
	Finished bool `json:"finished"`
	// Summary says what changed and why; for an unfinished result, what
	// remains.
	Summary string `json:"summary"`
	// CommitMessage is a conventional-commit subject and an optional body.
	CommitMessage string `json:"commit_message"`
	// Checklist are the items of the pull request's manual verification.
	Checklist []string `json:"checklist"`
}

// subject is the conventional-commit rule the harness holds a PR title to.
var subject = regexp.MustCompile(`^(feat|fix|docs|ci|chore|refactor|test|perf|build|style|revert)(\(([a-z0-9-]+)\))?!?: [^ ].*$`)

// Subject is the first line of the commit message: the pull request's title.
func (r Result) Subject() string {
	line, _, _ := strings.Cut(strings.TrimSpace(r.CommitMessage), "\n")
	return strings.TrimSpace(line)
}

// Check re-checks what the schema cannot express. scopes are the Target
// Project's commit scopes.
func (r Result) Check(scopes []string) error {
	if strings.TrimSpace(r.Summary) == "" {
		return errors.New("the summary is empty")
	}
	if !r.Finished {
		return nil
	}
	parts := subject.FindStringSubmatch(r.Subject())
	switch {
	case parts == nil:
		return fmt.Errorf("the commit subject %q is not <type>(<scope>): <description>", r.Subject())
	case parts[3] != "" && !slices.Contains(scopes, parts[3]):
		return fmt.Errorf("the commit scope %q is not one of the Target Project's (%s)", parts[3], strings.Join(scopes, ", "))
	case len(r.Checklist) == 0:
		return errors.New("a finished result has no checklist item")
	}
	for i, item := range r.Checklist {
		if strings.TrimSpace(item) == "" || strings.ContainsAny(item, "\r\n") {
			return fmt.Errorf("checklist item %d is not one line of text", i+1)
		}
	}
	return nil
}
