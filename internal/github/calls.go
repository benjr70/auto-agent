package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Repository is what the Daemon reads of a repository.
type Repository struct {
	// FullName is the repository's current owner/name: not what it was
	// asked for by, when it has been renamed or moved.
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	// Permissions are what the token may do here; nil when GitHub did not
	// say.
	Permissions *Permissions `json:"permissions"`
}

// Permissions are a token's rights on a repository.
type Permissions struct {
	Admin bool `json:"admin"`
	Push  bool `json:"push"`
	Pull  bool `json:"pull"`
}

// User is the account a token belongs to.
type User struct {
	Login string
	// Classic is whether the token is a classic one, which GitHub says by
	// naming its scopes; a fine-grained or app token has none to name.
	Classic bool
	Scopes  []string
}

// Issue is an issue, or the issue side of a pull request.
type Issue struct {
	// ID is the database id, which the sub-issue and dependency calls take
	// in place of the number.
	ID        int64
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
	ID     int64  `json:"id"`
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
	issue := Issue{ID: raw.ID, Number: raw.Number, Title: raw.Title, Body: raw.Body, State: raw.State, IsPullRequest: raw.PullRequest != nil}
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
	NodeID string
	Number int
	Title  string
	// Body is the description; a listing carries it too.
	Body   string
	State  string
	Draft  bool
	Merged bool
	// MergeSHA is the commit a merged pull request landed as; empty when
	// it is not merged.
	MergeSHA string
	HeadRef  string
	HeadSHA  string
	// HeadRepo is the repository the head branch lives in, as owner/name;
	// empty when that repository is gone.
	HeadRepo string
	BaseRef  string
	// Author is the login of whoever opened it.
	Author string
	Labels []string
	// CreatedAt is GitHub's ISO 8601 UTC timestamp, which sorts as text.
	CreatedAt string
	// MergedAt is when it was merged, in the same form; empty when it is
	// not merged.
	MergedAt string
	// Mergeable is whether it merges cleanly into its base. Only a pull
	// request read alone says; a listing leaves it Unknown.
	Mergeable Mergeability
	// URL is its page on GitHub.
	URL string
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

// User reads the account the token belongs to, and the token's scopes.
func (c *Client) User(ctx context.Context) (User, error) {
	var user struct {
		Login string `json:"login"`
	}
	out := &withHeader{out: &user}
	if err := c.get(ctx, "/user", out); err != nil {
		return User{}, err
	}
	u := User{Login: user.Login}
	if values, ok := out.header["X-Oauth-Scopes"]; ok {
		u.Classic = true
		for _, scope := range strings.Split(strings.Join(values, ","), ",") {
			if scope = strings.TrimSpace(scope); scope != "" {
				u.Scopes = append(u.Scopes, scope)
			}
		}
	}
	return u, nil
}

// FileContent reads one file of a repository at its default branch.
func (c *Client) FileContent(ctx context.Context, owner, repo, path string) ([]byte, error) {
	var raw struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	if err := c.get(ctx, repoPath(owner, repo)+"/contents/"+strings.Join(segments, "/"), &raw); err != nil {
		return nil, err
	}
	if raw.Type != "file" || raw.Encoding != "base64" {
		return nil, fmt.Errorf("github: %s in %s/%s is not a file GitHub gave the contents of (type %q, encoding %q)", path, owner, repo, raw.Type, raw.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(raw.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github: decoding %s in %s/%s: %w", path, owner, repo, err)
	}
	return data, nil
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

// issuesPage is how many issues one request asks for.
const issuesPage = 100

// OpenIssues lists every open issue carrying label, pull requests excluded,
// read page by page until a short one.
func (c *Client) OpenIssues(ctx context.Context, owner, repo, label string) ([]Issue, error) {
	issues := []Issue{}
	for page := 1; ; page++ {
		var raw []rawIssue
		path := fmt.Sprintf("%s/issues?state=open&per_page=%d&page=%d&labels=%s", repoPath(owner, repo), issuesPage, page, url.QueryEscape(label))
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if r.PullRequest == nil {
				issues = append(issues, r.issue())
			}
		}
		if len(raw) < issuesPage {
			return issues, nil
		}
	}
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

// EditIssueBody replaces an issue's body.
func (c *Client) EditIssueBody(ctx context.Context, owner, repo string, number int, body string) error {
	payload := struct {
		Body string `json:"body"`
	}{body}
	return c.send(ctx, http.MethodPatch, issuePath(owner, repo, number), payload, nil)
}

// AddAssignees assigns logins to an issue, keeping the ones it has.
func (c *Client) AddAssignees(ctx context.Context, owner, repo string, number int, logins ...string) error {
	body := struct {
		Assignees []string `json:"assignees"`
	}{logins}
	return c.send(ctx, http.MethodPost, issuePath(owner, repo, number)+"/assignees", body, nil)
}

// parentQuery reads an issue's GitHub parent: the issue it is a sub-issue of.
const parentQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) { parent { number title body } }
  }
}`

// Parent reads the issue an issue is a sub-issue of; ok is false when it is
// no one's.
func (c *Client) Parent(ctx context.Context, owner, repo string, number int) (parent Issue, ok bool, err error) {
	var data struct {
		Repository *struct {
			Issue *struct {
				Parent *struct {
					Number int    `json:"number"`
					Title  string `json:"title"`
					Body   string `json:"body"`
				} `json:"parent"`
			} `json:"issue"`
		} `json:"repository"`
	}
	if err := c.GraphQL(ctx, parentQuery, map[string]any{"owner": owner, "name": repo, "number": number}, &data); err != nil {
		return Issue{}, false, err
	}
	if data.Repository == nil || data.Repository.Issue == nil {
		return Issue{}, false, fmt.Errorf("github: %s/%s#%d was not in the answer", owner, repo, number)
	}
	p := data.Repository.Issue.Parent
	if p == nil {
		return Issue{}, false, nil
	}
	return Issue{Number: p.Number, Title: p.Title, Body: p.Body}, true, nil
}

// subIssuesPage is how many sub-issues one request asks for.
const subIssuesPage = 100

// SubIssues lists the sub-issues of an issue, open and closed, in the order
// they have under it.
func (c *Client) SubIssues(ctx context.Context, owner, repo string, number int) ([]Issue, error) {
	issues := []Issue{}
	for page := 1; ; page++ {
		var raw []rawIssue
		path := fmt.Sprintf("%s/sub_issues?per_page=%d&page=%d", issuePath(owner, repo, number), subIssuesPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			issues = append(issues, r.issue())
		}
		if len(raw) < subIssuesPage {
			return issues, nil
		}
	}
}

// AddSubIssue makes the issue whose database id is child a sub-issue of
// parent.
func (c *Client) AddSubIssue(ctx context.Context, owner, repo string, parent int, child int64) error {
	payload := struct {
		SubIssueID int64 `json:"sub_issue_id"`
	}{child}
	return c.send(ctx, http.MethodPost, issuePath(owner, repo, parent)+"/sub_issues", payload, nil)
}

// AddBlockedBy records, as GitHub's native dependency, that blocked is
// blocked by the issue whose database id is blocker.
func (c *Client) AddBlockedBy(ctx context.Context, owner, repo string, blocked int, blocker int64) error {
	payload := struct {
		IssueID int64 `json:"issue_id"`
	}{blocker}
	return c.send(ctx, http.MethodPost, issuePath(owner, repo, blocked)+"/dependencies/blocked_by", payload, nil)
}

// rawPullRequest is a pull request as the REST API returns it.
type rawPullRequest struct {
	NodeID string `json:"node_id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
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
	HTMLURL   string `json:"html_url"`
	// MergedAt is when it was merged; a listing says so only here.
	MergedAt string `json:"merged_at"`
	// MergeCommitSHA is the commit a merged pull request landed as.
	MergeCommitSHA string `json:"merge_commit_sha"`
	// Mergeable is null while GitHub works it out, and absent from a listing.
	Mergeable *bool `json:"mergeable"`
}

func (raw rawPullRequest) pullRequest() PullRequest {
	pr := PullRequest{
		NodeID: raw.NodeID, Number: raw.Number, Title: raw.Title, Body: raw.Body, State: raw.State, Draft: raw.Draft, Merged: raw.Merged || raw.MergedAt != "",
		HeadRef: raw.Head.Ref, HeadSHA: raw.Head.SHA, BaseRef: raw.Base.Ref,
		CreatedAt: raw.CreatedAt, MergedAt: raw.MergedAt, Labels: []string{}, Mergeable: MergeableUnknown, URL: raw.HTMLURL,
	}
	if raw.Head.Repo != nil {
		pr.HeadRepo = raw.Head.Repo.FullName
	}
	if pr.Merged {
		pr.MergeSHA = raw.MergeCommitSHA
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

// EditPullRequestBody replaces a pull request's description.
func (c *Client) EditPullRequestBody(ctx context.Context, owner, repo string, number int, body string) error {
	payload := struct {
		Body string `json:"body"`
	}{body}
	return c.send(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", repoPath(owner, repo), number), payload, nil)
}

// EditPullRequestTitle replaces a pull request's title.
func (c *Client) EditPullRequestTitle(ctx context.Context, owner, repo string, number int, title string) error {
	payload := struct {
		Title string `json:"title"`
	}{title}
	return c.send(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", repoPath(owner, repo), number), payload, nil)
}

// Commit is one commit a pull request carries, and who made it: the
// GitHub account, when GitHub matched one, and the name and email git
// recorded, and its message.
type Commit struct {
	SHA     string
	Login   string
	Name    string
	Email   string
	Message string
}

// commitsPage is how many commits one request asks for.
const commitsPage = 100

// PullCommits lists the commits a pull request carries, oldest first.
func (c *Client) PullCommits(ctx context.Context, owner, repo string, number int) ([]Commit, error) {
	commits := []Commit{}
	for page := 1; ; page++ {
		var raw []struct {
			SHA    string `json:"sha"`
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
			Commit struct {
				Author struct {
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"author"`
				Message string `json:"message"`
			} `json:"commit"`
		}
		path := fmt.Sprintf("%s/pulls/%d/commits?per_page=%d&page=%d", repoPath(owner, repo), number, commitsPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			commit := Commit{SHA: r.SHA, Name: r.Commit.Author.Name, Email: r.Commit.Author.Email, Message: r.Commit.Message}
			if r.Author != nil {
				commit.Login = r.Author.Login
			}
			commits = append(commits, commit)
		}
		if len(raw) < commitsPage {
			return commits, nil
		}
	}
}

// reviewsPage is how many reviews one request asks for.
const reviewsPage = 100

// Review is one review of a pull request: who left it, what it says, and
// the commit it was left on.
type Review struct {
	Author string
	// State is APPROVED, CHANGES_REQUESTED, COMMENTED or DISMISSED.
	State string
	// CommitID is the head the review was left on.
	CommitID string
}

// Reviews lists a pull request's reviews, oldest first.
func (c *Client) Reviews(ctx context.Context, owner, repo string, number int) ([]Review, error) {
	reviews := []Review{}
	for page := 1; ; page++ {
		var raw []struct {
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
			User     *struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		path := fmt.Sprintf("%s/pulls/%d/reviews?per_page=%d&page=%d", repoPath(owner, repo), number, reviewsPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			review := Review{State: r.State, CommitID: r.CommitID}
			if r.User != nil {
				review.Author = r.User.Login
			}
			reviews = append(reviews, review)
		}
		if len(raw) < reviewsPage {
			return reviews, nil
		}
	}
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

// MergedPullRequests lists the pull requests merged most recently: those
// among the last page of closed ones, by when each was last updated.
func (c *Client) MergedPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error) {
	var raw []rawPullRequest
	path := fmt.Sprintf("%s/pulls?state=closed&sort=updated&direction=desc&per_page=%d", repoPath(owner, repo), pullsPage)
	if err := c.get(ctx, path, &raw); err != nil {
		return nil, err
	}
	prs := []PullRequest{}
	for _, r := range raw {
		if pr := r.pullRequest(); pr.Merged {
			prs = append(prs, pr)
		}
	}
	return prs, nil
}

// MergePullRequest squash-merges a pull request, but only while its head is
// sha: a head that moved since it was read is not merged, and GitHub answers
// 409. It returns the commit the merge landed as.
func (c *Client) MergePullRequest(ctx context.Context, owner, repo string, number int, sha, title string) (string, error) {
	payload := struct {
		Method string `json:"merge_method"`
		SHA    string `json:"sha"`
		Title  string `json:"commit_title"`
	}{"squash", sha, title}
	var merged struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	if err := c.send(ctx, http.MethodPut, fmt.Sprintf("%s/pulls/%d/merge", repoPath(owner, repo), number), payload, &merged); err != nil {
		return "", err
	}
	if !merged.Merged {
		return "", fmt.Errorf("github: %s/%s#%d was not merged", owner, repo, number)
	}
	return merged.SHA, nil
}

// filesPage is how many changed files one request asks for.
const filesPage = 100

// Files lists the paths a pull request changes. A file it renames is both
// of its paths, as a diff with no rename detection has it: a file moved into
// a directory is still a change to the one it left.
func (c *Client) Files(ctx context.Context, owner, repo string, number int) ([]string, error) {
	paths := []string{}
	for page := 1; ; page++ {
		var raw []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		path := fmt.Sprintf("%s/pulls/%d/files?per_page=%d&page=%d", repoPath(owner, repo), number, filesPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, file := range raw {
			paths = append(paths, file.Filename)
			if file.PreviousFilename != "" {
				paths = append(paths, file.PreviousFilename)
			}
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
	// CreatedAt is GitHub's ISO 8601 UTC timestamp.
	CreatedAt string
}

// commentsPage is how many comments one request asks for.
const commentsPage = 100

// Comments lists every comment on an issue or pull request, oldest first.
func (c *Client) Comments(ctx context.Context, owner, repo string, number int) ([]Comment, error) {
	comments := []Comment{}
	for page := 1; ; page++ {
		var raw []struct {
			ID        int64  `json:"id"`
			Body      string `json:"body"`
			CreatedAt string `json:"created_at"`
			User      *struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		path := fmt.Sprintf("%s/comments?per_page=%d&page=%d", issuePath(owner, repo, number), commentsPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			comment := Comment{ID: r.ID, Body: r.Body, CreatedAt: r.CreatedAt}
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

// Label is a repository label.
type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// labelsPage is how many labels one request asks for.
const labelsPage = 100

// Labels lists the names of every label the repository has.
func (c *Client) Labels(ctx context.Context, owner, repo string) ([]string, error) {
	names := []string{}
	for page := 1; ; page++ {
		var raw []Label
		path := fmt.Sprintf("%s/labels?per_page=%d&page=%d", repoPath(owner, repo), labelsPage, page)
		if err := c.get(ctx, path, &raw); err != nil {
			return nil, err
		}
		for _, l := range raw {
			names = append(names, l.Name)
		}
		if len(raw) < labelsPage {
			return names, nil
		}
	}
}

// CreateLabel creates a label in the repository.
func (c *Client) CreateLabel(ctx context.Context, owner, repo string, label Label) error {
	return c.send(ctx, http.MethodPost, repoPath(owner, repo)+"/labels", label, nil)
}
