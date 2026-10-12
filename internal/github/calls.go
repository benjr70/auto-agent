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
	Number        int
	Title         string
	Body          string
	State         string
	Labels        []string
	Assignees     []string
	IsPullRequest bool
}

// PullRequest is a pull request.
type PullRequest struct {
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
}

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
	var raw struct {
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
		PullRequest *struct{} `json:"pull_request"`
	}
	if err := c.get(ctx, issuePath(owner, repo, number), &raw); err != nil {
		return Issue{}, err
	}
	issue := Issue{Number: raw.Number, Title: raw.Title, Body: raw.Body, State: raw.State, IsPullRequest: raw.PullRequest != nil}
	for _, l := range raw.Labels {
		issue.Labels = append(issue.Labels, l.Name)
	}
	for _, a := range raw.Assignees {
		issue.Assignees = append(issue.Assignees, a.Login)
	}
	return issue, nil
}

// rawPullRequest is a pull request as the REST API returns it.
type rawPullRequest struct {
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
}

func (raw rawPullRequest) pullRequest() PullRequest {
	pr := PullRequest{
		Number: raw.Number, Title: raw.Title, State: raw.State, Draft: raw.Draft, Merged: raw.Merged,
		HeadRef: raw.Head.Ref, HeadSHA: raw.Head.SHA, BaseRef: raw.Base.Ref,
	}
	if raw.Head.Repo != nil {
		pr.HeadRepo = raw.Head.Repo.FullName
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
