package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// Bucket is how a check stands, in the words `gh pr checks` sorts checks
// into, which the harness has always read.
type Bucket string

const (
	BucketPass     Bucket = "pass"
	BucketFail     Bucket = "fail"
	BucketPending  Bucket = "pending"
	BucketSkipping Bucket = "skipping"
	// BucketCancel: the run was cancelled before it said anything.
	BucketCancel Bucket = "cancel"
)

// Check is one check on a commit: a check run, or a commit status.
type Check struct {
	Name   string
	Bucket Bucket
	// Link is where the check's page is.
	Link string
	// Job is the GitHub Actions job behind a check run, whose log can be
	// read; 0 for anything else.
	Job int64
}

// actionsApp is the GitHub App that runs GitHub Actions jobs: its check
// runs are jobs, and their ids are job ids.
const actionsApp = "github-actions"

// runBucket sorts a check run the way `gh pr checks` does.
func runBucket(status, conclusion string) Bucket {
	if status != "completed" {
		return BucketPending
	}
	switch conclusion {
	case "success":
		return BucketPass
	case "skipped", "neutral":
		return BucketSkipping
	case "cancelled":
		return BucketCancel
	case "failure", "timed_out", "action_required", "startup_failure", "stale":
		return BucketFail
	}
	return BucketPending
}

// statusBucket sorts a commit status the way `gh pr checks` does.
func statusBucket(state string) Bucket {
	switch state {
	case "success":
		return BucketPass
	case "failure", "error":
		return BucketFail
	}
	return BucketPending
}

// Checks reads every check on a commit: its check runs, the latest of each,
// and its commit statuses, the latest for each context.
func (c *Client) Checks(ctx context.Context, owner, repo, sha string) ([]Check, error) {
	checks := []Check{}
	for page := 1; ; page++ {
		var raw struct {
			CheckRuns []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				HTMLURL    string `json:"html_url"`
				App        *struct {
					Slug string `json:"slug"`
				} `json:"app"`
			} `json:"check_runs"`
		}
		path := fmt.Sprintf("%s/commits/%s/check-runs?filter=latest&per_page=%d&page=%d", repoPath(owner, repo), url.PathEscape(sha), commentsPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw.CheckRuns {
			check := Check{Name: r.Name, Bucket: runBucket(r.Status, r.Conclusion), Link: r.HTMLURL}
			if r.App != nil && r.App.Slug == actionsApp {
				check.Job = r.ID
			}
			checks = append(checks, check)
		}
		if len(raw.CheckRuns) < commentsPage {
			break
		}
	}
	var combined struct {
		Statuses []struct {
			Context   string `json:"context"`
			State     string `json:"state"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}
	if err := c.get(ctx, fmt.Sprintf("%s/commits/%s/status?per_page=%d", repoPath(owner, repo), url.PathEscape(sha), commentsPage), &combined); err != nil {
		return nil, err
	}
	for _, s := range combined.Statuses {
		checks = append(checks, Check{Name: s.Context, Bucket: statusBucket(s.State), Link: s.TargetURL})
	}
	return checks, nil
}

// JobLog reads the log of a GitHub Actions job.
func (c *Client) JobLog(ctx context.Context, owner, repo string, job int64) (string, error) {
	var data []byte
	if err := c.get(ctx, fmt.Sprintf("%s/actions/jobs/%d/logs", repoPath(owner, repo), job), &data); err != nil {
		return "", err
	}
	return string(data), nil
}

// RerunFailedJobs runs again the failed jobs of a GitHub Actions workflow
// run, on the same commit.
func (c *Client) RerunFailedJobs(ctx context.Context, owner, repo string, run int64) error {
	return c.send(ctx, http.MethodPost, fmt.Sprintf("%s/actions/runs/%d/rerun-failed-jobs", repoPath(owner, repo), run), nil, nil)
}

// runLink finds the workflow run in a GitHub Actions job's page link.
var runLink = regexp.MustCompile(`/actions/runs/([0-9]+)/job`)

// RunOf is the GitHub Actions workflow run a job's page link names; 0 when
// it names none.
func RunOf(link string) int64 {
	m := runLink.FindStringSubmatch(link)
	if m == nil {
		return 0
	}
	run, _ := strconv.ParseInt(m[1], 10, 64)
	return run
}

const draftMutation = `
mutation($id: ID!) {
  convertPullRequestToDraft(input: {pullRequestId: $id}) {
    pullRequest { isDraft }
  }
}`

// MarkDraft turns a pull request back into a draft.
func (c *Client) MarkDraft(ctx context.Context, pr PullRequest) error {
	return c.GraphQL(ctx, draftMutation, map[string]any{"id": pr.NodeID}, nil)
}
