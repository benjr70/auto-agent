package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Repository is what the Daemon reads of a repository.
type Repository struct {
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
	BaseRef string
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

// PullRequest reads one pull request.
func (c *Client) PullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Draft  bool   `json:"draft"`
		Merged bool   `json:"merged"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.get(ctx, fmt.Sprintf("%s/pulls/%d", repoPath(owner, repo), number), &raw); err != nil {
		return PullRequest{}, err
	}
	return PullRequest{
		Number: raw.Number, Title: raw.Title, State: raw.State, Draft: raw.Draft, Merged: raw.Merged,
		HeadRef: raw.Head.Ref, HeadSHA: raw.Head.SHA, BaseRef: raw.Base.Ref,
	}, nil
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
