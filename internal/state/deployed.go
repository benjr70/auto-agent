package state

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/checklist"
	"github.com/benjr70/auto-agent/internal/github"
)

// The Deployed tier's pick, as lib/deployed-tier.sh had it.
const (
	// DeployedWait is the wait window: how long a merged Agent PR must have
	// been quiet, since it merged and since its last round, before a round
	// is run on it. Its deploy has had time to land, and the Daemon, which
	// cycles again straight after a Fire that worked, does not spend every
	// round on one PR inside a few minutes.
	DeployedWait = 30 * time.Minute
	// DeployedLookBack is how many of the most recently merged PRs the pick
	// looks back over.
	DeployedLookBack = 50
	// defaultManualVerify is the schema default of rounds.manual_verify.
	defaultManualVerify = 3
)

// Deployed is the merged Agent PR the Deployed tier works next, with the
// round it is owed and the deferred items it runs.
type Deployed struct {
	PR     int    `json:"pr"`
	Issue  int    `json:"issue"`
	Title  string `json:"title"`
	Branch string `json:"branch"`
	// MergedAt is when it merged, as GitHub says it.
	MergedAt string `json:"mergedAt"`
	// Round is the round this is; Max is rounds.manual_verify.
	Round int `json:"round"`
	Max   int `json:"max"`
	// Items are its deferred items: unticked and post-deploy tagged.
	Items []checklist.Item `json:"items"`
}

// PickDeployed is the merged Agent PR the Deployed tier works next: of the
// DeployedLookBack most recently merged, the oldest merged feat/issue-<n> PR
// the Daemon, login, opened that has deferred items and a round left under
// limit, once it has been quiet for wait since it merged and since its last
// round. Rounds are the Daemon's own comments under the Deployed heading,
// an infrastructure error's included. comments reads a PR's comments, and
// is asked only about the PRs every other rule leaves, oldest first, until
// one is picked. A PR whose comments cannot be read is passed over, as its
// rounds cannot be counted; warnings say which. nil is no pick.
func PickDeployed(merged []github.PullRequest, comments func(pr int) ([]github.Comment, error), login string, limit int, now time.Time, wait time.Duration) (pick *Deployed, warnings []string) {
	if limit < 1 {
		limit = defaultManualVerify
	}
	recent := slices.Clone(merged)
	slices.SortStableFunc(recent, func(a, b github.PullRequest) int { return cmp.Compare(b.MergedAt, a.MergedAt) })
	recent = recent[:min(len(recent), DeployedLookBack)]
	slices.Reverse(recent)
	quiet := now.Add(-wait)
	for _, pr := range recent {
		m := sliceBranch.FindStringSubmatch(pr.HeadRef)
		if m == nil || !pr.Merged || !Ours(pr, login) || stamp(pr.MergedAt).After(quiet) {
			continue
		}
		items := checklist.Deferred(pr.Body)
		if len(items) == 0 {
			continue
		}
		list, err := comments(pr.Number)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("the Deployed tier rounds of PR #%d could not be counted: %v", pr.Number, err))
			continue
		}
		rounds, last := 0, time.Time{}
		for _, c := range list {
			if !strings.Contains(c.Body, checklist.DeployedHeading) || (login != "" && !strings.EqualFold(c.Author, login)) {
				continue
			}
			rounds++
			if at := stamp(c.CreatedAt); at.After(last) {
				last = at
			}
		}
		if rounds >= limit || last.After(quiet) {
			continue
		}
		return &Deployed{PR: pr.Number, Issue: atoi(m[1]), Title: pr.Title, Branch: pr.HeadRef, MergedAt: pr.MergedAt, Round: rounds + 1, Max: limit, Items: items}, warnings
	}
	return nil, warnings
}

// stamp is GitHub's timestamp; one it does not give, or that cannot be read,
// is long ago.
func stamp(at string) time.Time {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return time.Time{}
	}
	return t
}
