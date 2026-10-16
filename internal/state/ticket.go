package state

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	parentHeading = regexp.MustCompile(`^##\s*Parent( PRD)?\b`)
	issueRef      = regexp.MustCompile(`#(\d+)`)
)

// ParentOf is the ticket's parent Spec: the first #N in its body's
// `## Parent` section (`## Parent PRD` on tickets older than the word Spec);
// 0 when the body has no such section, or it names no issue.
func ParentOf(body string) int {
	in := false
	for _, line := range strings.Split(body, "\n") {
		switch {
		case parentHeading.MatchString(line):
			in = true
		case strings.HasPrefix(line, "## "):
			in = false
		case in:
			if m := issueRef.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[1])
				return n
			}
		}
	}
	return 0
}
