package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Repository is what the Daemon reads of a repository.
type Repository struct {
	// FullName is the repository's current owner/name: not what it was
	// asked for by, when it has been renamed or moved.
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

// Issue is an issue, or the issue side of a pull request.
type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string
	Labels    []string
	Assignees []string
	// Author is who opened it; empty when that account is gone.
	Author        string
	IsPullRequest bool
}

// rawIssue is an issue as the REST API returns it.
type rawIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
	PullRequest *struct{} `json:"pull_request"`
}

func (raw rawIssue) issue() Issue {
	issue := Issue{Number: raw.Number, Title: raw.Title, Body: raw.Body, State: raw.State, IsPullRequest: raw.PullRequest != nil}
	for _, l := range raw.Labels {
		issue.Labels = append(issue.Labels, l.Name)
	}
	for _, a := range raw.Assignees {
		issue.Assignees = append(issue.Assignees, a.Login)
	}
	if raw.User != nil {
		issue.Author = raw.User.Login
	}
	return issue
}

// PullRequest is a pull request.
type PullRequest struct {
	// NodeID is its GraphQL id, which marking it draft takes.
	NodeID  string
	Number  int
	Title   string
	State   string
	Draft   bool
	Merged  bool
	HeadRef string
	HeadSHA string
	// HeadRepo is the repository the head branch lives in, as owner/name;
	// empty when that repository is gone.
	HeadRepo string
	BaseRef  string
	// Author is the login of whoever opened it.
	Author string
	Labels []string
	// CreatedAt is GitHub's ISO 8601 UTC timestamp, which sorts as text.
	CreatedAt string
	// Mergeable is whether it merges cleanly into its base. Only a pull
	// request read alone says; a listing leaves it Unknown.
	Mergeable Mergeability
}

// Mergeability is whether a pull request merges cleanly, in GitHub's GraphQL
// words.
type Mergeability string

const (
	Mergeable   Mergeability = "MERGEABLE"
	Conflicting Mergeability = "CONFLICTING"
	// MergeableUnknown: GitHub is still working it out, in the background.
	// Reading the pull request is what sets it going.
	MergeableUnknown Mergeability = "UNKNOWN"
)

func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

func issuePath(owner, repo string, number int) string {
	return fmt.Sprintf("%s/issues/%d", repoPath(owner, repo), number)
}

// Login is the login of the account the token belongs to: the identity the
// Daemon acts as.
func (c *Client) Login(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := c.get(ctx, "/user", &user); err != nil {
		return "", err
	}
	return user.Login, nil
}

// Repository reads a repository.
func (c *Client) Repository(ctx context.Context, owner, repo string) (Repository, error) {
	var r Repository
	err := c.get(ctx, repoPath(owner, repo), &r)
	return r, err
}

// Issue reads one issue.
func (c *Client) Issue(ctx context.Context, owner, repo string, number int) (Issue, error) {
	var raw rawIssue
	if err := c.get(ctx, issuePath(owner, repo, number), &raw); err != nil {
		return Issue{}, err
	}
	return raw.issue(), nil
}

// OpenIssues lists the open issues carrying label, pull requests excluded:
// up to the first hundred, which is far more than the harness labels it is
// asked about are ever on.
func (c *Client) OpenIssues(ctx context.Context, owner, repo, label string) ([]Issue, error) {
	var raw []rawIssue
	path := repoPath(owner, repo) + "/issues?state=open&per_page=100&labels=" + url.QueryEscape(label)
	if err := c.get(ctx, path, &raw); err != nil {
		return nil, err
	}
	issues := []Issue{}
	for _, r := range raw {
		if r.PullRequest == nil {
			issues = append(issues, r.issue())
		}
	}
	return issues, nil
}

// NewIssue is an issue to open.
type NewIssue struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

// CreateIssue opens an issue.
func (c *Client) CreateIssue(ctx context.Context, owner, repo string, issue NewIssue) (Issue, error) {
	var raw rawIssue
	if err := c.send(ctx, http.MethodPost, repoPath(owner, repo)+"/issues", issue, &raw); err != nil {
		return Issue{}, err
	}
	return raw.issue(), nil
}

// CloseIssue closes an issue as completed.
func (c *Client) CloseIssue(ctx context.Context, owner, repo string, number int) error {
	payload := struct {
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
	}{"closed", "completed"}
	return c.send(ctx, http.MethodPatch, issuePath(owner, repo, number), payload, nil)
}

// rawPullRequest is a pull request as the REST API returns it.
type rawPullRequest struct {
	NodeID string `json:"node_id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Merged bool   `json:"merged"`
	Head   struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	CreatedAt string `json:"created_at"`
	// Mergeable is null while GitHub works it out, and absent from a listing.
	Mergeable *bool `json:"mergeable"`
}

func (raw rawPullRequest) pullRequest() PullRequest {
	pr := PullRequest{
		NodeID: raw.NodeID, Number: raw.Number, Title: raw.Title, State: raw.State, Draft: raw.Draft, Merged: raw.Merged,
		HeadRef: raw.Head.Ref, HeadSHA: raw.Head.SHA, BaseRef: raw.Base.Ref,
		CreatedAt: raw.CreatedAt, Labels: []string{}, Mergeable: MergeableUnknown,
	}
	if raw.Head.Repo != nil {
		pr.HeadRepo = raw.Head.Repo.FullName
	}
	if raw.User != nil {
		pr.Author = raw.User.Login
	}
	for _, label := range raw.Labels {
		pr.Labels = append(pr.Labels, label.Name)
	}
	switch {
	case raw.Mergeable == nil:
	case *raw.Mergeable:
		pr.Mergeable = Mergeable
	default:
		pr.Mergeable = Conflicting
	}
	return pr
}

// PullRequest reads one pull request.
func (c *Client) PullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	var raw rawPullRequest
	if err := c.get(ctx, fmt.Sprintf("%s/pulls/%d", repoPath(owner, repo), number), &raw); err != nil {
		return PullRequest{}, err
	}
	return raw.pullRequest(), nil
}

// pullsPage is how many pull requests one request asks for.
const pullsPage = 100

// OpenPullRequests lists every open pull request of the repository.
func (c *Client) OpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error) {
	prs := []PullRequest{}
	for page := 1; ; page++ {
		var raw []rawPullRequest
		path := fmt.Sprintf("%s/pulls?state=open&per_page=%d&page=%d", repoPath(owner, repo), pullsPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, pr := range raw {
			prs = append(prs, pr.pullRequest())
		}
		if len(raw) < pullsPage {
			return prs, nil
		}
	}
}

// filesPage is how many changed files one request asks for.
const filesPage = 100

// Files lists the paths a pull request changes.
func (c *Client) Files(ctx context.Context, owner, repo string, number int) ([]string, error) {
	paths := []string{}
	for page := 1; ; page++ {
		var raw []struct {
			Filename string `json:"filename"`
		}
		path := fmt.Sprintf("%s/pulls/%d/files?per_page=%d&page=%d", repoPath(owner, repo), number, filesPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, file := range raw {
			paths = append(paths, file.Filename)
		}
		if len(raw) < filesPage {
			return paths, nil
		}
	}
}

// ReviewComment is one comment on a line of a pull request's diff: the
// first comment of a review thread, or a reply in one.
type ReviewComment struct {
	ID     int64  `json:"id,omitempty"`
	Body   string `json:"body"`
	Path   string `json:"path"`
	Author string `json:"-"`
	// Line is the line it is on, on Side of the diff: RIGHT, the head, or
	// LEFT, the base. 0 when the line has since left the diff.
	Line int    `json:"line,omitempty"`
	Side string `json:"side,omitempty"`
	// CommitID is the commit the line is read at, when posting.
	CommitID string `json:"commit_id,omitempty"`
}

// CreateReviewComment opens a review thread on a line of a pull request's
// diff. GitHub answers 422 when the line is not in the diff.
func (c *Client) CreateReviewComment(ctx context.Context, owner, repo string, number int, comment ReviewComment) error {
	return c.send(ctx, http.MethodPost, fmt.Sprintf("%s/pulls/%d/comments", repoPath(owner, repo), number), comment, nil)
}

// ReviewComments lists every comment on the lines of a pull request's diff,
// oldest first.
func (c *Client) ReviewComments(ctx context.Context, owner, repo string, number int) ([]ReviewComment, error) {
	comments := []ReviewComment{}
	for page := 1; ; page++ {
		var raw []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			Path string `json:"path"`
			Line *int   `json:"line"`
			Side string `json:"side"`
			User *struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		path := fmt.Sprintf("%s/pulls/%d/comments?per_page=%d&page=%d", repoPath(owner, repo), number, commentsPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			comment := ReviewComment{ID: r.ID, Body: r.Body, Path: r.Path, Side: r.Side}
			if r.Line != nil {
				comment.Line = *r.Line
			}
			if r.User != nil {
				comment.Author = r.User.Login
			}
			comments = append(comments, comment)
		}
		if len(raw) < commentsPage {
			return comments, nil
		}
	}
}

// NewPullRequest is what opening a pull request takes.
type NewPullRequest struct {
	Title string `json:"title"`
	// Head is the branch holding the change; Base the branch it merges into.
	Head string `json:"head"`
	Base string `json:"base"`
	Body string `json:"body"`
}

// CreatePullRequest opens a pull request.
func (c *Client) CreatePullRequest(ctx context.Context, owner, repo string, pr NewPullRequest) (PullRequest, error) {
	var raw rawPullRequest
	if err := c.send(ctx, http.MethodPost, repoPath(owner, repo)+"/pulls", pr, &raw); err != nil {
		return PullRequest{}, err
	}
	return raw.pullRequest(), nil
}

// AddLabels adds labels to an issue or pull request, keeping the ones it has.
func (c *Client) AddLabels(ctx context.Context, owner, repo string, number int, labels ...string) error {
	body := struct {
		Labels []string `json:"labels"`
	}{labels}
	return c.send(ctx, http.MethodPost, issuePath(owner, repo, number)+"/labels", body, nil)
}

// RemoveLabel takes one label off an issue or pull request. A label that is
// not there is already removed, so that is not an error.
func (c *Client) RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error {
	err := c.send(ctx, http.MethodDelete, issuePath(owner, repo, number)+"/labels/"+url.PathEscape(label), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// CreateComment posts a comment on an issue or pull request.
func (c *Client) CreateComment(ctx context.Context, owner, repo string, number int, body string) error {
	payload := struct {
		Body string `json:"body"`
	}{body}
	return c.send(ctx, http.MethodPost, issuePath(owner, repo, number)+"/comments", payload, nil)
}

// Comment is one comment on an issue or pull request.
type Comment struct {
	ID     int64
	Author string
	Body   string
}

// commentsPage is how many comments one request asks for.
const commentsPage = 100

// Comments lists every comment on an issue or pull request, oldest first.
func (c *Client) Comments(ctx context.Context, owner, repo string, number int) ([]Comment, error) {
	comments := []Comment{}
	for page := 1; ; page++ {
		var raw []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User *struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		path := fmt.Sprintf("%s/comments?per_page=%d&page=%d", issuePath(owner, repo, number), commentsPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			comment := Comment{ID: r.ID, Body: r.Body}
			if r.User != nil {
				comment.Author = r.User.Login
			}
			comments = append(comments, comment)
		}
		if len(raw) < commentsPage {
			return comments, nil
		}
	}
}

// UpdateComment replaces the body of a comment on an issue or pull request.
func (c *Client) UpdateComment(ctx context.Context, owner, repo string, id int64, body string) error {
	payload := struct {
		Body string `json:"body"`
	}{body}
	return c.send(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/comments/%d", repoPath(owner, repo), id), payload, nil)
}
