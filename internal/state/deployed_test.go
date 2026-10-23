package state

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/github"
)

// Ported from lib/deployed-tier.test.sh ("the pick is the oldest merged
// Agent PR with deferred items and rounds left"), under its case names.
// Dropped: "merged PRs of the configured repo are listed", as the listing
// is the GitHub client's (the Fire's test reads it); "an unreadable PR list
// is no pick, and says so" is the Fire's too, where the list is read. Now is
// pinned an hour after the last canned timestamp, as there.
func TestPickDeployed(t *testing.T) {
	now := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	const (
		tagged = "## Manual verification\n\n- [ ] live check <!-- post-deploy: GET /api/health -->\n"
		round  = "### Deployed verification — round 1/3"
	)
	merged := func(number int, title, branch, at, body string) github.PullRequest {
		return github.PullRequest{Number: number, Title: title, HeadRef: branch, MergedAt: at, Merged: true, Body: body, Author: login}
	}
	byLogin := func(body, at string) github.Comment { return github.Comment{Author: login, Body: body, CreatedAt: at} }
	prs := []github.PullRequest{
		merged(60, "newest", "feat/issue-12", "2026-09-20T00:00:00Z", tagged),
		merged(49, "merged minutes ago", "feat/issue-3", "2026-09-20T00:50:00Z", tagged),
		merged(48, "round minutes ago", "feat/issue-2", "2026-08-01T00:00:00Z", tagged),
		merged(51, "human branch", "fix-typo", "2026-09-01T00:00:00Z", tagged),
		merged(52, "exhausted", "feat/issue-7", "2026-09-02T00:00:00Z", tagged),
		merged(53, "nothing deferred", "feat/issue-8", "2026-09-03T00:00:00Z", "## Manual verification\n\n- [ ] local only\n"),
		merged(55, "the one", "feat/issue-9", "2026-09-05T00:00:00Z", tagged),
	}
	comments := map[int][]github.Comment{
		60: {byLogin(round, "")},
		48: {byLogin(round, "2026-09-20T00:45:00Z")},
		52: {byLogin(round, ""), byLogin(round, ""), byLogin(round, "")},
		55: {byLogin("unrelated", ""), byLogin(round, "")},
	}
	var asked []int
	read := func(pr int) ([]github.Comment, error) { asked = append(asked, pr); return comments[pr], nil }

	t.Run("skips a non-Agent branch, an exhausted PR, one with nothing deferred and two inside the wait window; the round is the next one", func(t *testing.T) {
		asked = nil
		got, warnings := PickDeployed(prs, read, login, 3, now, DeployedWait)
		want := Deployed{PR: 55, Issue: 9, Title: "the one", Branch: "feat/issue-9", MergedAt: "2026-09-05T00:00:00Z", Round: 2, Max: 3,
			Items: []checklist.Item{{Section: checklist.Manual, Text: "live check <!-- post-deploy: GET /api/health -->", PostDeploy: "GET /api/health"}}}
		if got == nil || got.PR != want.PR || got.Issue != want.Issue || got.Title != want.Title || got.Branch != want.Branch || got.MergedAt != want.MergedAt ||
			got.Round != want.Round || got.Max != want.Max || !slices.Equal(got.Items, want.Items) || len(warnings) != 0 {
			t.Fatalf("PickDeployed = %+v, %v", got, warnings)
		}
		// Oldest first, and only the PRs every other rule left: no comment
		// is read for the human's branch, nor for one with nothing deferred
		// or merged inside the window.
		if !slices.Equal(asked, []int{48, 52, 55}) {
			t.Errorf("comments read of %v", asked)
		}
	})
	t.Run("a shorter wait window lets the PR whose last round was 15 minutes ago back in", func(t *testing.T) {
		got, _ := PickDeployed(prs, read, login, 3, now, 5*time.Minute)
		if got == nil || got.PR != 48 || got.Round != 2 {
			t.Errorf("PickDeployed = %+v", got)
		}
	})
	t.Run("nothing deferred anywhere: exit 1", func(t *testing.T) {
		if got, _ := PickDeployed(nil, read, login, 3, now, DeployedWait); got != nil {
			t.Errorf("PickDeployed = %+v", got)
		}
	})
	t.Run("a round anyone else heads is not the Daemon's, and does not count", func(t *testing.T) {
		pr := merged(70, "x", "feat/issue-70", "2026-09-01T00:00:00Z", tagged)
		someone := func(int) ([]github.Comment, error) {
			return []github.Comment{{Author: "someone", Body: round}, {Author: "someone", Body: round}, {Author: "someone", Body: round}}, nil
		}
		if got, _ := PickDeployed([]github.PullRequest{pr}, someone, login, 3, now, DeployedWait); got == nil || got.Round != 1 {
			t.Errorf("PickDeployed = %+v", got)
		}
	})
	t.Run("a PR someone else opened, or not merged, is not the Lane's", func(t *testing.T) {
		theirs := merged(71, "x", "feat/issue-71", "2026-09-01T00:00:00Z", tagged)
		theirs.Author = "someone"
		open := merged(72, "x", "feat/issue-72", "", tagged)
		open.Merged = false
		if got, _ := PickDeployed([]github.PullRequest{theirs, open}, read, login, 3, now, DeployedWait); got != nil {
			t.Errorf("PickDeployed = %+v", got)
		}
	})
	t.Run("a PR whose rounds cannot be counted is passed over, and says so", func(t *testing.T) {
		broken := merged(73, "x", "feat/issue-73", "2026-09-01T00:00:00Z", tagged)
		fine := merged(74, "y", "feat/issue-74", "2026-09-02T00:00:00Z", tagged)
		flaky := func(pr int) ([]github.Comment, error) {
			if pr == 73 {
				return nil, errors.New("boom")
			}
			return nil, nil
		}
		got, warnings := PickDeployed([]github.PullRequest{fine, broken}, flaky, login, 3, now, DeployedWait)
		if got == nil || got.PR != 74 || len(warnings) != 1 || !strings.Contains(warnings[0], "PR #73") {
			t.Errorf("PickDeployed = %+v, %v", got, warnings)
		}
	})
	t.Run("it looks back over the last 50 merged PRs only", func(t *testing.T) {
		var many []github.PullRequest
		old := merged(1, "the oldest", "feat/issue-1", "2026-01-01T00:00:00Z", tagged)
		for i := 0; i < DeployedLookBack; i++ {
			many = append(many, merged(100+i, "untagged", "feat/issue-5", time.Date(2026, 9, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339), "no checklist"))
		}
		if got, _ := PickDeployed(append(many, old), read, login, 3, now, DeployedWait); got != nil {
			t.Errorf("PickDeployed = %+v", got)
		}
	})
}

func TestDeriveDeployed(t *testing.T) {
	d := &Deployed{PR: 55, Issue: 9, Round: 1, Max: 3}
	t.Run("the Deployed tier is last of the Lanes: taken only when nothing else is owed", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: labelsPick, Deployed: d})
		if want := (Next{Action: ActionDeployed, Issue: 9, PR: 55}); got.Kind != KindDeployed || got.Next != want || got.Deployed != d {
			t.Errorf("Derive = %+v", got)
		}
		got = Derive(Facts{Login: login, Pick: labelsPick, Deployed: d, Queue: []github.QueueIssue{labelIssue(11, "slice", "2026-01-02")}})
		if got.Kind != KindPick || got.Deployed != nil {
			t.Errorf("a pick goes first: Derive = %+v", got)
		}
		got = Derive(Facts{Login: login, Pick: labelsPick, Deployed: d, Paused: []Paused{{Issue: 4, Pauses: 1}}})
		if got.Kind != KindResume {
			t.Errorf("paused work goes first: Derive = %+v", got)
		}
	})
}
