// Package pick puts a ticket on the Target Project's pick signal and takes
// it off: the AFK label, and under a Project pick the ticket's item on the
// board with its Priority. The planning skills reach it through
// `auto-agent pick`; the Graduate fog Step reuses it.
package pick

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/daemon"
	"github.com/benjr70/auto-agent/internal/github"
)

// Label is the label every pick shape picks on.
const Label = "AFK"

// GitHub is what the pick signal needs of GitHub (github.Client).
type GitHub interface {
	daemon.Labeler
	AddLabels(ctx context.Context, owner, repo string, number int, labels ...string) error
	RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error
	GraphQL(ctx context.Context, query string, variables map[string]any, out any) error
}

// Why a ticket did not land on the pick signal, or did not leave it.
const (
	// ReasonUnknownPriority is a Priority outside the configured order,
	// refused before any write.
	ReasonUnknownPriority = "unknown-priority"
	ReasonLabelsFailed    = "labels-failed"
	ReasonLabelFailed     = "label-failed"
	ReasonIssueUnreadable = "issue-unreadable"
	// ReasonProjectUnreadable is the board, or its fields, not read.
	ReasonProjectUnreadable = "project-unreadable"
	ReasonFieldMissing      = "field-missing"
	ReasonOptionMissing     = "option-missing"
	ReasonItemAddFailed     = "item-add-failed"
	// ReasonPriorityEditFailed is the item on the board with no Priority
	// set: a caller must report it, never assume the Priority landed.
	ReasonPriorityEditFailed = "priority-edit-failed"
	ReasonItemListUnreadable = "item-list-unreadable"
	// ReasonItemListTruncated is the issue on more boards than one read
	// returns, so its absence from this one cannot be proven.
	ReasonItemListTruncated = "item-list-truncated"
	ReasonItemDeleteFailed  = "item-delete-failed"
)

// Error is a pick that did not land, with the reason it names.
type Error struct {
	Reason string
	Err    error
}

func (e *Error) Error() string { return e.Reason + ": " + e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

func fail(reason string, format string, args ...any) *Error {
	return &Error{Reason: reason, Err: fmt.Errorf(format, args...)}
}

// Result is what Add or Remove did.
type Result struct {
	Issue int
	// Shape is the pick shape it acted under (config.PickProject or
	// config.PickLabels).
	Shape string
	// LabelsCreated are the harness labels Add found missing and created.
	LabelsCreated []string
	// ItemID is the ticket's item on the board, when it has or had one.
	ItemID string
	// Priority is the value Add set under a Project pick.
	Priority string
	// OnBoard is, for Add, the item on the board; for Remove, an item
	// taken off it.
	OnBoard bool
}

// Add puts issue on the pick signal: the harness labels ensured first, then
// under a Project pick the item added and its Priority set (priority empty
// is the first of the configured order), then the AFK label. The label goes
// on last, so a ticket whose board write failed is never picked. Under a
// label-only pick, priority is ignored. A failure is an *Error.
func Add(ctx context.Context, gh GitHub, harness config.Harness, issue int, priority string) (Result, error) {
	repo := harness.Repo
	result := Result{Issue: issue, Shape: harness.Pick.Shape}
	project := harness.Pick.Project
	if harness.Pick.Shape == config.PickProject {
		if priority == "" && len(project.Order) > 0 {
			priority = project.Order[0]
		}
		if !slices.Contains(project.Order, priority) {
			return result, fail(ReasonUnknownPriority, "priority %q is not in the configured order %q", priority, project.Order)
		}
	}
	created, _, err := daemon.EnsureLabels(ctx, gh, repo.Owner, repo.Name)
	result.LabelsCreated = created
	if err != nil {
		return result, &Error{Reason: ReasonLabelsFailed, Err: err}
	}
	if harness.Pick.Shape == config.PickProject {
		board, err := readBoard(ctx, gh, repo, issue, project, priority)
		if err != nil {
			return result, err
		}
		var added struct {
			AddProjectV2ItemByID *struct {
				Item *struct {
					ID string `json:"id"`
				} `json:"item"`
			} `json:"addProjectV2ItemById"`
		}
		err = gh.GraphQL(ctx, addItemMutation, map[string]any{"project": board.projectID, "content": board.issueID}, &added)
		if err == nil && (added.AddProjectV2ItemByID == nil || added.AddProjectV2ItemByID.Item == nil) {
			err = errors.New("GitHub returned no item")
		}
		if err != nil {
			return result, fail(ReasonItemAddFailed, "adding #%d to project %d: %w", issue, project.Number, err)
		}
		result.ItemID, result.OnBoard = added.AddProjectV2ItemByID.Item.ID, true
		err = gh.GraphQL(ctx, setPriorityMutation, map[string]any{
			"project": board.projectID, "item": result.ItemID, "field": board.fieldID, "option": board.optionID,
		}, nil)
		if err != nil {
			return result, fail(ReasonPriorityEditFailed, "#%d is on project %d but its %s could not be set to %s: %w", issue, project.Number, project.PriorityField, priority, err)
		}
		result.Priority = priority
	}
	if err := gh.AddLabels(ctx, repo.Owner, repo.Name, issue, Label); err != nil {
		return result, fail(ReasonLabelFailed, "labelling #%d %s: %w", issue, Label, err)
	}
	return result, nil
}

// Remove takes issue off the pick signal: the AFK label first, so that it
// is no longer picked whatever else fails, then under a Project pick its
// item off the board. A ticket that is not on the board is no failure.
func Remove(ctx context.Context, gh GitHub, harness config.Harness, issue int) (Result, error) {
	repo := harness.Repo
	result := Result{Issue: issue, Shape: harness.Pick.Shape}
	if err := gh.RemoveLabel(ctx, repo.Owner, repo.Name, issue, Label); err != nil {
		return result, fail(ReasonLabelFailed, "taking %s off #%d: %w", Label, issue, err)
	}
	if harness.Pick.Shape != config.PickProject {
		return result, nil
	}
	project := harness.Pick.Project
	var data struct {
		Repository *struct {
			Issue *struct {
				ProjectItems *struct {
					Nodes []struct {
						ID      string `json:"id"`
						Project *struct {
							ID string `json:"id"`
						} `json:"project"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
				} `json:"projectItems"`
			} `json:"issue"`
			Owner *struct {
				ProjectV2 *struct {
					ID string `json:"id"`
				} `json:"projectV2"`
			} `json:"owner"`
		} `json:"repository"`
	}
	err := gh.GraphQL(ctx, itemsQuery, map[string]any{
		"owner": repo.Owner, "name": repo.Name, "issue": issue, "number": project.Number,
	}, &data)
	if partial := (*github.GraphQLError)(nil); errors.As(err, &partial) && partial.Partial {
		err = nil // what is missing is judged below
	}
	switch {
	case err != nil:
		return result, fail(ReasonItemListUnreadable, "reading the boards of #%d: %w", issue, err)
	case data.Repository == nil || data.Repository.Owner == nil || data.Repository.Owner.ProjectV2 == nil:
		return result, fail(ReasonProjectUnreadable, "project %d of %s not found", project.Number, repo.Owner)
	case data.Repository.Issue == nil || data.Repository.Issue.ProjectItems == nil:
		return result, fail(ReasonItemListUnreadable, "the boards of #%d could not be read", issue)
	}
	projectID := data.Repository.Owner.ProjectV2.ID
	items := data.Repository.Issue.ProjectItems
	for _, item := range items.Nodes {
		if item.Project != nil && item.Project.ID == projectID {
			result.ItemID = item.ID
		}
	}
	if result.ItemID == "" {
		if items.PageInfo.HasNextPage {
			return result, fail(ReasonItemListTruncated, "#%d is on more boards than one read returns", issue)
		}
		return result, nil
	}
	if err := gh.GraphQL(ctx, deleteItemMutation, map[string]any{"project": projectID, "item": result.ItemID}, nil); err != nil {
		return result, fail(ReasonItemDeleteFailed, "taking #%d off project %d: %w", issue, project.Number, err)
	}
	result.OnBoard = true
	return result, nil
}

// board is the ids a Project write needs.
type board struct {
	issueID, projectID, fieldID, optionID string
}

// readBoard reads, in one query, the issue's node id and the project's, its
// priority field's and the option for priority.
func readBoard(ctx context.Context, gh GitHub, repo config.Repo, issue int, project *config.Project, priority string) (board, error) {
	var data struct {
		Repository *struct {
			Issue *struct {
				ID string `json:"id"`
			} `json:"issue"`
			Owner *struct {
				ProjectV2 *struct {
					ID    string `json:"id"`
					Field *struct {
						ID      string `json:"id"`
						Options []struct {
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"options"`
					} `json:"field"`
				} `json:"projectV2"`
			} `json:"owner"`
		} `json:"repository"`
	}
	err := gh.GraphQL(ctx, boardQuery, map[string]any{
		"owner": repo.Owner, "name": repo.Name, "issue": issue, "number": project.Number, "field": project.PriorityField,
	}, &data)
	if partial := (*github.GraphQLError)(nil); errors.As(err, &partial) && partial.Partial {
		err = nil // what is missing is judged below
	}
	if err != nil {
		return board{}, fail(ReasonProjectUnreadable, "reading #%d and project %d: %w", issue, project.Number, err)
	}
	if data.Repository == nil || data.Repository.Issue == nil {
		return board{}, fail(ReasonIssueUnreadable, "#%d not found in %s", issue, repo.Slug())
	}
	if data.Repository.Owner == nil || data.Repository.Owner.ProjectV2 == nil {
		return board{}, fail(ReasonProjectUnreadable, "project %d of %s not found", project.Number, repo.Owner)
	}
	p := data.Repository.Owner.ProjectV2
	// A field of another type has no id under the single-select fragment.
	if p.Field == nil || p.Field.ID == "" {
		return board{}, fail(ReasonFieldMissing, "project %d has no single-select field %q", project.Number, project.PriorityField)
	}
	b := board{issueID: data.Repository.Issue.ID, projectID: p.ID, fieldID: p.Field.ID}
	for _, option := range p.Field.Options {
		if option.Name == priority {
			b.optionID = option.ID
		}
	}
	if b.optionID == "" {
		return board{}, fail(ReasonOptionMissing, "project %d's %s field has no option %q", project.Number, project.PriorityField, priority)
	}
	return b, nil
}

const boardQuery = `query($owner: String!, $name: String!, $issue: Int!, $number: Int!, $field: String!) {
  repository(owner: $owner, name: $name) {
    issue(number: $issue) { id }
    owner {
      ... on ProjectV2Owner {
        projectV2(number: $number) {
          id
          field(name: $field) {
            ... on ProjectV2SingleSelectField { id options { id name } }
          }
        }
      }
    }
  }
}`

const itemsQuery = `query($owner: String!, $name: String!, $issue: Int!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $issue) {
      projectItems(first: 100, includeArchived: true) {
        nodes { id project { id } }
        pageInfo { hasNextPage }
      }
    }
    owner {
      ... on ProjectV2Owner { projectV2(number: $number) { id } }
    }
  }
}`

const addItemMutation = `mutation($project: ID!, $content: ID!) {
  addProjectV2ItemById(input: {projectId: $project, contentId: $content}) { item { id } }
}`

const setPriorityMutation = `mutation($project: ID!, $item: ID!, $field: ID!, $option: String!) {
  updateProjectV2ItemFieldValue(input: {projectId: $project, itemId: $item, fieldId: $field, value: {singleSelectOptionId: $option}}) {
    projectV2Item { id }
  }
}`

const deleteItemMutation = `mutation($project: ID!, $item: ID!) {
  deleteProjectV2Item(input: {projectId: $project, itemId: $item}) { deletedItemId }
}`
