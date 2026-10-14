package state

import (
	"strings"
	"testing"
)

func TestImplemented(t *testing.T) {
	const done = "feat(a): add the greeting\n\nIt says hello.\n\nCloses #20\n"
	for _, tt := range []struct {
		name     string
		messages []string
		want     string
	}{
		{"a branch with no commits is not implemented", nil, ""},
		{"a branch of wip commits is not implemented", []string{"wip: partial work on #20\n\nhalf\n", "wip: freeze partial work on #20 (usage exhausted)\n"}, ""},
		{"the finished Implement commit, closing the ticket, makes it implemented", []string{"wip: partial work on #20\n", done, "fix(a): cover the empty list\n"}, done},
		{"a commit closing another ticket does not", []string{"feat(a): x\n\nCloses #200\n"}, ""},
		{"a wip commit that mentions the closing line does not", []string{"wip: partial work on #20\n\nCloses #20\n"}, ""},
		{"the closing line is matched as git and GitHub match it, in any case", []string{"feat(a): x\n\ncloses #20\n"}, "feat(a): x\n\ncloses #20\n"},
		{"the first closing commit is the Implement commit", []string{done, "feat(a): again\n\nCloses #20\n"}, done},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := Implemented(20, tt.messages); got != tt.want || ok != (tt.want != "") {
				t.Errorf("Implemented = %q, %v", got, ok)
			}
		})
	}
}

func TestPreCommitCapComment(t *testing.T) {
	got := PreCommitCapComment(10, []string{"cover the empty-list case", "drop the mock of ItemStore"})
	for _, want := range []string{"10 rounds", "`AFK:failed` applied", "- cover the empty-list case\n- drop the mock of ItemStore"} {
		if !strings.Contains(got, want) {
			t.Errorf("the comment lacks %q:\n%s", want, got)
		}
	}
}
