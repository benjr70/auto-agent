package github

import (
	"context"
	"fmt"
	"net/http"
)

// ReviewThread is one unresolved review thread on a pull request, read
// whole: its first comment and every reply, oldest first.
type ReviewThread struct {
	// ID is the thread's GraphQL node id, which resolving it takes.
	ID   string
	Path string
	// Line is the line it is on; 0 when the line has left the diff.
	Line     int
	Comments []ThreadComment
}

// ThreadComment is one comment of a review thread.
type ThreadComment struct {
	// ID is the comment's REST id, which a reply names.
	ID     int64
	Author string
	Body   string
}

const threadsQuery = `
query($owner: String!, $name: String!, $pr: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          path
          line
          comments(first: 100) {
            pageInfo { hasNextPage endCursor }
            nodes { databaseId body author { login } }
          }
        }
      }
    }
  }
}`

const threadCommentsQuery = `
query($id: ID!, $after: String!) {
  node(id: $id) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { databaseId body author { login } }
      }
    }
  }
}`

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type rawThreadComments struct {
	PageInfo *pageInfo `json:"pageInfo"`
	Nodes    []struct {
		DatabaseID int64  `json:"databaseId"`
		Body       string `json:"body"`
		Author     *struct {
			Login string `json:"login"`
		} `json:"author"`
	} `json:"nodes"`
}

func (raw rawThreadComments) comments() []ThreadComment {
	comments := make([]ThreadComment, 0, len(raw.Nodes))
	for _, n := range raw.Nodes {
		comment := ThreadComment{ID: n.DatabaseID, Body: n.Body}
		if n.Author != nil {
			comment.Author = n.Author.Login
		}
		comments = append(comments, comment)
	}
	return comments
}

// next is the cursor of the page after this one; empty on the last.
func (p *pageInfo) next() string {
	if p == nil || !p.HasNextPage {
		return ""
	}
	return p.EndCursor
}

// ReviewThreads reads every unresolved review thread on a pull request,
// each one whole: a thread longer than one page is followed page by page.
// A resolved thread is left out: it is not open for anyone.
func (c *Client) ReviewThreads(ctx context.Context, owner, repo string, number int) ([]ReviewThread, error) {
	threads := []ReviewThread{}
	for after := ""; ; {
		variables := map[string]any{"owner": owner, "name": repo, "pr": number}
		if after != "" {
			variables["after"] = after
		}
		var data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						PageInfo *pageInfo `json:"pageInfo"`
						Nodes    []struct {
							ID         string            `json:"id"`
							IsResolved bool              `json:"isResolved"`
							Path       string            `json:"path"`
							Line       *int              `json:"line"`
							Comments   rawThreadComments `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.GraphQL(ctx, threadsQuery, variables, &data); err != nil {
			return nil, err
		}
		if data.Repository == nil || data.Repository.PullRequest == nil {
			return nil, fmt.Errorf("github: the review threads of %s/%s#%d were not in the answer", owner, repo, number)
		}
		list := data.Repository.PullRequest.ReviewThreads
		for _, node := range list.Nodes {
			if node.IsResolved {
				continue
			}
			thread := ReviewThread{ID: node.ID, Path: node.Path, Comments: node.Comments.comments()}
			if node.Line != nil {
				thread.Line = *node.Line
			}
			for cursor := node.Comments.PageInfo.next(); cursor != ""; {
				var more struct {
					Node *struct {
						Comments rawThreadComments `json:"comments"`
					} `json:"node"`
				}
				if err := c.GraphQL(ctx, threadCommentsQuery, map[string]any{"id": node.ID, "after": cursor}, &more); err != nil {
					return nil, err
				}
				if more.Node == nil {
					return nil, fmt.Errorf("github: review thread %s was not in the answer", node.ID)
				}
				thread.Comments = append(thread.Comments, more.Node.Comments.comments()...)
				cursor = more.Node.Comments.PageInfo.next()
			}
			threads = append(threads, thread)
		}
		if after = list.PageInfo.next(); after == "" {
			return threads, nil
		}
	}
}

// ReplyToThread replies in a review thread, to its first comment, so that
// the answer lands in the thread and not as a comment on the pull request.
func (c *Client) ReplyToThread(ctx context.Context, owner, repo string, number int, comment int64, body string) error {
	payload := struct {
		Body      string `json:"body"`
		InReplyTo int64  `json:"in_reply_to"`
	}{body, comment}
	return c.send(ctx, http.MethodPost, fmt.Sprintf("%s/pulls/%d/comments", repoPath(owner, repo), number), payload, nil)
}

const resolveThreadMutation = `
mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`

// ResolveThread marks a review thread resolved.
func (c *Client) ResolveThread(ctx context.Context, thread string) error {
	return c.GraphQL(ctx, resolveThreadMutation, map[string]any{"threadId": thread}, nil)
}
