package state

import (
	"regexp"
	"strconv"
	"strings"
)

// PreCommitReviewCap is how many rounds of pre-commit review a ticket gets
// in one Fire: a fixed harness constant, never configured.
const PreCommitReviewCap = 10

// Implemented finds the ticket's finished Implement commit among the
// messages of the commits its branch carries, oldest first: the first that
// closes the ticket and is not a wip commit. A branch that carries one is
// past Implement and goes to the pre-commit review.
func Implemented(issue int, messages []string) (message string, ok bool) {
	closes := regexp.MustCompile(`(?im)^Closes #` + strconv.Itoa(issue) + `\s*$`)
	for _, m := range messages {
		if !strings.HasPrefix(strings.ToLower(m), "wip:") && closes.MatchString(m) {
			return m, true
		}
	}
	return "", false
}
