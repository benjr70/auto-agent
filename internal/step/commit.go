package step

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// conventional is the conventional-commit rule the harness holds a commit
// subject and a PR title to.
var conventional = regexp.MustCompile(`^(feat|fix|docs|ci|chore|refactor|test|perf|build|style|revert)(\(([a-z0-9-]+)\))?!?: [^ ].*$`)

// Subject is the first line of a commit message.
func Subject(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	return strings.TrimSpace(line)
}

// CheckSubject checks a commit subject a Step answered with: conventional,
// with a scope from the Target Project's commit scopes when it has one.
func CheckSubject(subject string, scopes []string) error {
	parts := conventional.FindStringSubmatch(subject)
	switch {
	case parts == nil:
		return fmt.Errorf("the commit subject %q is not <type>(<scope>): <description>", subject)
	case parts[3] != "" && !slices.Contains(scopes, parts[3]):
		return fmt.Errorf("the commit scope %q is not one of the Target Project's (%s)", parts[3], strings.Join(scopes, ", "))
	}
	return nil
}
