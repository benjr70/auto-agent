package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// QueueIssue is one open ticket of the queue, with everything a pick is
// decided on.
type QueueIssue struct {
	// Incomplete names what GitHub left out of this issue in a partial
	// response ("blockers", "labels and assignees"); empty when it is whole.
	// What is missing reads as empty below, so a caller must not take an
	// incomplete issue's empty lists at their word.
	Incomplete string
	Number     int
	Title      string
	// CreatedAt is GitHub's ISO 8601 UTC timestamp, which sorts as text.
	CreatedAt string
	Labels    []string
	// Blockers are the issue's native dependencies: the first page of them.
	Blockers []QueueBlocker
	// BlockersTruncated is set when there are more blockers than the page
	// holds, so an open one may be unseen.
	BlockersTruncated bool
	Assignees         []string
	// ProjectItems are the boards the issue is on; empty when the queue was
	// read without a priority field.
	ProjectItems []QueueProjectItem
}

// QueueBlocker is one issue blocking another.
type QueueBlocker struct {
	Number int
	// State is GitHub's: "OPEN" or "CLOSED".
	State string
}

// QueueProjectItem is an issue's place on one Project board.
type QueueProjectItem struct {
	// Project is the board's number.
	Project int
	// Priority is the value of the priority field; empty when it is unset.
	Priority string
}

const queueQueryHead = `query($owner: String!, $name: String!, $label: String!%s) {
  repository(owner: $owner, name: $name) {
    issues(first: 100, labels: [$label], states: OPEN) {
      nodes {
        number
        title
        createdAt
        labels(first: 30) { nodes { name } }
        blockedBy(first: 50) { nodes { number state } pageInfo { hasNextPage } }
        assignees(first: 10) { nodes { login } }%s
      }
    }
  }
}`

const queueProjectFragment = `
        projectItems(first: 10) {
          nodes {
            project { number }
            fieldValueByName(name: $field) {
              ... on ProjectV2ItemFieldSingleSelectValue { name }
            }
          }
        }`

// Queue reads the open issues carrying label, in one call. priorityField
// names the Project field to read for each board the issue is on; empty
// leaves boards out of the query, which then needs no project scope.
//
// GitHub can answer with data and errors together. That is not a failure:
// the issues that came back are returned, each marked Incomplete where a
// list is missing, and the errors with them as warnings, so one unreadable
// field never empties the queue.
func (c *Client) Queue(ctx context.Context, owner, repo, label, priorityField string) (issues []QueueIssue, warnings []string, err error) {
	variables := map[string]any{"owner": owner, "name": repo, "label": label}
	declaration, fragment := "", ""
	if priorityField != "" {
		variables["field"] = priorityField
		declaration, fragment = ", $field: String!", queueProjectFragment
	}
	var data struct {
		Repository *struct {
			Issues *struct {
				Nodes []struct {
					Number    int    `json:"number"`
					Title     string `json:"title"`
					CreatedAt string `json:"createdAt"`
					Labels    *struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
					BlockedBy *struct {
						Nodes []struct {
							Number int    `json:"number"`
							State  string `json:"state"`
						} `json:"nodes"`
						PageInfo *struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
					} `json:"blockedBy"`
					Assignees *struct {
						Nodes []struct {
							Login string `json:"login"`
						} `json:"nodes"`
					} `json:"assignees"`
					ProjectItems *struct {
						Nodes []struct {
							Project *struct {
								Number int `json:"number"`
							} `json:"project"`
							FieldValueByName *struct {
								Name string `json:"name"`
							} `json:"fieldValueByName"`
						} `json:"nodes"`
					} `json:"projectItems"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"repository"`
	}
	err = c.GraphQL(ctx, fmt.Sprintf(queueQueryHead, declaration, fragment), variables, &data)
	var gql *GraphQLError
	switch {
	case errors.As(err, &gql) && gql.Partial:
		for _, item := range gql.Errors {
			warnings = append(warnings, item.Message)
		}
	case err != nil:
		return nil, nil, err
	}
	if data.Repository == nil || data.Repository.Issues == nil {
		// Data came back, but not the queue: that is not an empty queue.
		return nil, nil, fmt.Errorf("github: the queue of %s/%s was not returned: %w", owner, repo, err)
	}
	for _, node := range data.Repository.Issues.Nodes {
		issue := QueueIssue{Number: node.Number, Title: node.Title, CreatedAt: node.CreatedAt}
		var missing []string
		if node.Labels == nil || node.Labels.Nodes == nil {
			missing = append(missing, "labels")
		}
		if node.BlockedBy == nil || node.BlockedBy.Nodes == nil {
			missing = append(missing, "blockers")
		}
		if node.Assignees == nil || node.Assignees.Nodes == nil {
			missing = append(missing, "assignees")
		}
		if priorityField != "" && (node.ProjectItems == nil || node.ProjectItems.Nodes == nil) {
			missing = append(missing, "project items")
		}
		issue.Incomplete = joinAnd(missing)
		if node.Labels != nil {
			for _, l := range node.Labels.Nodes {
				issue.Labels = append(issue.Labels, l.Name)
			}
		}
		if b := node.BlockedBy; b != nil {
			for _, blocker := range b.Nodes {
				issue.Blockers = append(issue.Blockers, QueueBlocker{Number: blocker.Number, State: blocker.State})
			}
			issue.BlockersTruncated = b.PageInfo != nil && b.PageInfo.HasNextPage
		}
		if node.Assignees != nil {
			for _, a := range node.Assignees.Nodes {
				issue.Assignees = append(issue.Assignees, a.Login)
			}
		}
		if node.ProjectItems != nil {
			for _, item := range node.ProjectItems.Nodes {
				if item.Project == nil {
					continue
				}
				entry := QueueProjectItem{Project: item.Project.Number}
				if item.FieldValueByName != nil {
					entry.Priority = item.FieldValueByName.Name
				}
				issue.ProjectItems = append(issue.ProjectItems, entry)
			}
		}
		issues = append(issues, issue)
	}
	return issues, warnings, nil
}

// joinAnd is "a", "a and b" or "a, b and c".
func joinAnd(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// OpenIssuesLabelled lists the numbers of the open issues carrying label,
// pull requests excluded: up to the first hundred, which is far more than
// the lock labels it is asked about are ever on.
func (c *Client) OpenIssuesLabelled(ctx context.Context, owner, repo, label string) ([]int, error) {
	var raw []struct {
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	}
	path := repoPath(owner, repo) + "/issues?state=open&per_page=100&labels=" + url.QueryEscape(label)
	if err := c.get(ctx, path, &raw); err != nil {
		return nil, err
	}
	numbers := []int{}
	for _, issue := range raw {
		if issue.PullRequest == nil {
			numbers = append(numbers, issue.Number)
		}
	}
	return numbers, nil
}
