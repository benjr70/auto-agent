package fire

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/state"
	"github.com/benjr70/auto-agent/internal/step/research"
)

// How long the PR Triage waits for GitHub to work out whether an Agent PR
// merges. Reading a pull request is what sets the work going after the
// default branch moves, so asking again soon usually has the answer: about
// two minutes at most, then whatever GitHub said last is taken.
const (
	mergeableRetries  = 6
	mergeableInterval = 20 * time.Second
)

// Sleep waits for d, or until ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// triageReads reads what the PR Triage needs beyond the listing of the pull
// requests from the repository's own branches, for the Agent PRs it could
// take: whether each merges, whether its review and
// verification finished, whether it is research docs only, and whether the
// human answered its Ruling request. Only what could change the verdict is
// read. A read that fails leaves its signal unread, which the triage takes
// the safe way, and says so in a warning.
func (s Source) triageReads(ctx context.Context, own []github.PullRequest) (prs []state.OpenPR, warnings []string, err error) {
	prs = make([]state.OpenPR, len(own))
	for i, pr := range own {
		prs[i] = state.OpenPR{PullRequest: pr}
	}
	comments := map[int][]github.Comment{}
	readComments := func(number int) ([]github.Comment, bool) {
		if list, read := comments[number]; read {
			return list, true
		}
		list, err := s.GitHub.Comments(ctx, s.Repo.Owner, s.Repo.Name, number)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("the comments of PR #%d could not be read: %v", number, err))
			return nil, false
		}
		comments[number] = list
		return list, true
	}
	var unknown []int
	for i, pr := range prs {
		if !state.Ours(pr.PullRequest, s.Login) {
			continue
		}
		open := &prs[i]
		if slices.Contains(pr.Labels, state.LabelRuling) {
			// Parked or not: the human's Ruling is the one way back in.
			if list, read := readComments(pr.Number); read {
				pending, err := ruling.Read(list, s.Login)
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("the Ruling request on PR #%d could not be read: %v", pr.Number, err))
				} else {
					owed := pending.Owed()
					open.RulingOwed = &owed
				}
			}
		}
		if !s.clean(*open) {
			continue
		}
		if read := s.readMergeable(ctx, open, &warnings); read && open.Mergeable == github.MergeableUnknown {
			unknown = append(unknown, i)
		}
	}
	for try := 0; len(unknown) > 0 && try < mergeableRetries; try++ {
		sleep := s.Sleep
		if sleep == nil {
			sleep = Sleep
		}
		if err := sleep(ctx, mergeableInterval); err != nil {
			return nil, warnings, err
		}
		still := unknown[:0]
		for _, i := range unknown {
			if read := s.readMergeable(ctx, &prs[i], &warnings); read && prs[i].Mergeable == github.MergeableUnknown {
				still = append(still, i)
			}
		}
		unknown = still
	}
	for i := range prs {
		open := &prs[i]
		if !s.clean(*open) {
			continue
		}
		// Clean so far: did the review and verification finish, and is it
		// a research PR?
		if list, read := readComments(open.Number); read {
			review, verify := false, state.VerifyFinished(list, s.Login)
			for _, c := range list {
				review = review || state.IsReviewDone(c.Body)
			}
			open.ReviewDone, open.VerifyDone = &review, &verify
		}
		if prefix := s.Harness.DocsResearchPrefix; prefix != "" {
			files, err := s.GitHub.Files(ctx, s.Repo.Owner, s.Repo.Name, open.Number)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("the files of PR #%d could not be read: %v", open.Number, err))
				continue
			}
			docsOnly := research.DocsOnly(files, prefix)
			open.DocsOnly = &docsOnly
		}
	}
	if state.Triage(prs, s.Login) == nil {
		// No Agent PR is taken, so a Bot PR could be.
		s.botReads(ctx, prs, &warnings)
	}
	return prs, warnings, nil
}

// dependabotConfig is where a Target Project configures Dependabot's
// version updates. With none, every Bot PR is a security update.
const dependabotConfig = ".github/dependabot.yml"

// botReads reads what the PR Triage needs about each Bot PR it could take:
// whether it merges, whether it is a security bump, whether the Daemon has
// committed to its branch, whether the deps gate refused its head for a
// human's reason, and, for one a human was handed with HITL, whether they
// approved its head. A Bot PR with
// a signal that decides whether it is taken left unread is not taken; one
// whose commits are unread counts as carrying the Daemon's, so that it is
// rebased rather than handed to a Dependabot that would refuse. With the
// Bot PR Lane off nothing is read, and the record says why each is left
// alone.
func (s Source) botReads(ctx context.Context, prs []state.OpenPR, warnings *[]string) {
	var configured *bool
	for i := range prs {
		open := &prs[i]
		if !state.IsBotPR(open.PullRequest) || state.Parked(open.PullRequest) || open.HeadSHA == "" {
			continue
		}
		if off := state.DepsLaneOff(s.Harness.Lanes.DepsLand); off != "" {
			*warnings = append(*warnings, state.NotTakenWarning(open.Number, off))
			continue
		}
		signals := state.BotSignals{}
		if slices.Contains(open.Labels, state.LabelHITL) {
			reviews, err := s.GitHub.Reviews(ctx, s.Repo.Owner, s.Repo.Name, open.Number)
			if err != nil {
				*warnings = append(*warnings, fmt.Sprintf("the reviews of Bot PR #%d could not be read: %v", open.Number, err))
				continue
			}
			if !state.ReviewOf(reviews, s.Login, open.HeadSHA).Approved {
				continue
			}
			signals.Approved = true
		}
		if configured == nil {
			_, err := s.GitHub.FileContent(ctx, s.Repo.Owner, s.Repo.Name, dependabotConfig)
			if err != nil && !github.IsNotFound(err) {
				*warnings = append(*warnings, fmt.Sprintf("whether %s exists could not be read, so no Bot PR is taken: %v", dependabotConfig, err))
				return
			}
			found := err == nil
			configured = &found
		}
		if read := s.readMergeable(ctx, open, warnings); !read {
			continue
		}
		if open.Mergeable != github.Conflicting {
			comments, err := s.GitHub.Comments(ctx, s.Repo.Owner, s.Repo.Name, open.Number)
			if err != nil {
				*warnings = append(*warnings, fmt.Sprintf("the comments of Bot PR #%d could not be read, so whether the deps gate refused its head is unknown and it is not taken: %v", open.Number, err))
				continue
			}
			history, err := state.ReadDepsHistory(comments, s.Login, open.HeadSHA)
			if err != nil {
				*warnings = append(*warnings, fmt.Sprintf("Bot PR #%d is not taken: %v", open.Number, err))
				continue
			}
			if history.Refused != "" {
				// Taking it again would hold the reconcile Lane, and every
				// pick, on a refusal only a human or a new head answers.
				*warnings = append(*warnings, state.RefusedWarning(open.Number, history.Refused))
				continue
			}
		}
		commits, err := s.GitHub.PullCommits(ctx, s.Repo.Owner, s.Repo.Name, open.Number)
		if err != nil {
			*warnings = append(*warnings, fmt.Sprintf("the commits of Bot PR #%d could not be read, so they count as the Daemon's: %v", open.Number, err))
			signals.AgentCommits = true
		} else {
			signals.AgentCommits = state.AgentCommits(commits)
		}
		signals.Security = state.IsSecurity(open.Body, *configured)
		open.Bot = &signals
	}
}

// clean says an Agent PR is not parked and has no reason yet: what else is
// read about it could still give it one.
func (s Source) clean(open state.OpenPR) bool {
	return state.Ours(open.PullRequest, s.Login) && !state.Parked(open.PullRequest) && state.ReasonOf(open, s.Login) == ""
}

// readMergeable reads the pull request alone, which is how GitHub says
// whether it merges. One that cannot be read stays Unknown, which is never
// a conflict, and is not asked again.
func (s Source) readMergeable(ctx context.Context, open *state.OpenPR, warnings *[]string) (read bool) {
	pr, err := s.GitHub.PullRequest(ctx, s.Repo.Owner, s.Repo.Name, open.Number)
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("whether PR #%d merges could not be read: %v", open.Number, err))
		return false
	}
	open.Mergeable = pr.Mergeable
	return true
}

// Source is where a Fire reads its facts from.
type Source struct {
	GitHub *github.Client
	Repo   config.Repo
	// Login is the account the Daemon acts as.
	Login   string
	Harness config.Harness
	// Live says another Fire of this Daemon is running.
	Live bool
	// Sleep waits between reads while GitHub works something out.
	Sleep func(context.Context, time.Duration) error
	// Now is the time the Deployed tier's wait window is measured to; nil
	// is the clock's.
	Now func() time.Time
}

// mergedResearch are the Daemon's research PRs merged while the ticket
// their marker names is still open in the queue, by ticket: their research
// is on the default branch, and only the ticket is owed. A PR labelled
// AFK:failed is left out: its ticket could not be finished from it. They
// are read only when the queue holds a research ticket.
func (s Source) mergedResearch(ctx context.Context, queue []github.QueueIssue) (map[int]int, error) {
	open := map[int]bool{}
	for _, issue := range queue {
		if state.WayfinderType(issue.Labels) == "research" {
			open[issue.Number] = true
		}
	}
	if len(open) == 0 {
		return nil, nil
	}
	merged, err := s.GitHub.MergedPullRequests(ctx, s.Repo.Owner, s.Repo.Name)
	if err != nil {
		return nil, err
	}
	tickets := map[int]int{}
	for _, pr := range ownBranches(merged, s.Repo) {
		// One labelled AFK:failed could not finish its ticket: a human
		// takes it from there, and it no longer holds the Lane.
		if !state.IsResearch(pr.HeadRef) || !state.Ours(pr, s.Login) || slices.Contains(pr.Labels, state.LabelFailed) {
			continue
		}
		if m, ok := research.ParseMarker(pr.Body); ok && open[m.Ticket] {
			if _, seen := tickets[m.Ticket]; !seen {
				tickets[m.Ticket] = pr.Number
			}
		}
	}
	return tickets, nil
}
