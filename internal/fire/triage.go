package fire

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
	"github.com/benjr70/auto-agent/internal/ruling"
	"github.com/benjr70/auto-agent/internal/state"
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
					open.RulingReplied = &owed
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
			review, verify := false, false
			for _, c := range list {
				review = review || state.IsReviewDone(c.Body)
				verify = verify || state.IsVerifyRound(c.Body)
			}
			open.ReviewDone, open.VerifyDone = &review, &verify
		}
		if prefix := s.Harness.DocsResearchPrefix; prefix != "" {
			files, err := s.GitHub.Files(ctx, s.Repo.Owner, s.Repo.Name, open.Number)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("the files of PR #%d could not be read: %v", open.Number, err))
				continue
			}
			docsOnly := docsResearchOnly(files, prefix)
			open.DocsOnly = &docsOnly
		}
	}
	return prs, warnings, nil
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

// docsResearchOnly is the one definition of a research docs PR: it changes
// at least one file, and every one is under the research docs prefix.
func docsResearchOnly(files []string, prefix string) bool {
	for _, file := range files {
		if !strings.HasPrefix(file, prefix) {
			return false
		}
	}
	return len(files) > 0
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
}
