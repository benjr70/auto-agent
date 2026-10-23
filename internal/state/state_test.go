package state

import (
	"reflect"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/config"
	"github.com/benjr70/auto-agent/internal/github"
)

// The case names are the ones lib/pickup-triage.test.sh uses for the lock
// and the pick.

const login = "agent-login"

var (
	projectPick = config.Pick{Shape: config.PickProject, Project: &config.Project{Number: 1, PriorityField: "Priority", Order: []string{"P0", "P1", "P2"}}}
	labelsPick  = config.Pick{Shape: config.PickLabels}
)

type opt func(*github.QueueIssue)

func labels(names ...string) opt {
	return func(i *github.QueueIssue) { i.Labels = append([]string{LabelAFK}, names...) }
}
func blockedBy(states ...string) opt {
	return func(i *github.QueueIssue) {
		for n, s := range states {
			i.Blockers = append(i.Blockers, github.QueueBlocker{Number: 98 + n, State: s})
		}
	}
}
func truncatedBlockers() opt { return func(i *github.QueueIssue) { i.BlockersTruncated = true } }
func assignees(logins ...string) opt {
	return func(i *github.QueueIssue) { i.Assignees = logins }
}
func offProject() opt {
	return func(i *github.QueueIssue) { i.ProjectItems[0].Project = 9 }
}

// issue is a queue entry on project 1; priority "" is an unset field.
func issue(number int, title, priority, created string, opts ...opt) github.QueueIssue {
	i := github.QueueIssue{
		Number: number, Title: title, CreatedAt: created + "T00:00:00Z", Labels: []string{LabelAFK},
		ProjectItems: []github.QueueProjectItem{{Project: 1, Priority: priority}},
	}
	for _, o := range opts {
		o(&i)
	}
	return i
}

// labelIssue is what the label-only query returns: no project items.
func labelIssue(number int, title, created string, opts ...opt) github.QueueIssue {
	i := issue(number, title, "", created, opts...)
	i.ProjectItems = nil
	return i
}

func TestDeriveLock(t *testing.T) {
	queue := []github.QueueIssue{issue(20, "clean", "P1", "2026-02-01")}
	for _, pick := range []config.Pick{projectPick, labelsPick} {
		t.Run(pick.Shape+" pick: AFK:in-progress held: verdict in-flight, no pick query", func(t *testing.T) {
			got := Derive(Facts{Login: login, Pick: pick, Held: []int{7, 9}, LiveFire: true, Queue: queue})
			if got.Kind != KindInFlight || got.InFlight != 2 || got.Pick != nil || got.Next.Action != ActionNone {
				t.Errorf("Derive = %+v", got)
			}
		})
	}
	t.Run("unreadable lock: fails safe to in-flight", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: projectPick, LockUnreadable: true, Queue: queue})
		if got.Kind != KindInFlight || got.Pick != nil || got.InFlight != 1 {
			t.Errorf("Derive = %+v", got)
		}
	})
}

func TestDerivePick(t *testing.T) {
	tests := []struct {
		name         string
		pick         config.Pick
		login        string
		queue        []github.QueueIssue
		wantKind     Kind
		wantIssue    int
		wantPriority string
		wantType     string
		wantSkipped  []string // a fragment of each Skipped reason, in order
	}{
		// project pick order and blocking
		{
			name: "open native blocker skips P0; off-project skipped; P1 picked",
			queue: []github.QueueIssue{
				issue(10, "blocked-p0", "P0", "2026-01-01", blockedBy("OPEN")),
				issue(20, "clean-p1", "P1", "2026-02-01"),
				issue(30, "orphan-p0", "P0", "2026-01-01", offProject()),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
		},
		{
			name:     "all native blockers CLOSED: candidate picked",
			queue:    []github.QueueIssue{issue(10, "unblocked-p0", "P0", "2026-01-01", blockedBy("CLOSED", "CLOSED"))},
			wantKind: KindPick, wantIssue: 10, wantPriority: "P0",
		},
		{
			name: "P0>P1>P2 (missing = P2) then oldest wins",
			queue: []github.QueueIssue{
				issue(40, "no-prio", "", "2025-01-01"),
				issue(50, "p1-newer", "P1", "2026-03-01"),
				issue(60, "p1-older", "P1", "2026-02-01"),
				issue(70, "p2", "P2", "2026-01-01"),
			},
			wantKind: KindPick, wantIssue: 60, wantPriority: "P1",
		},
		{
			name:     "a missing priority reports the last value of the order",
			queue:    []github.QueueIssue{issue(40, "no-prio", "", "2025-01-01")},
			wantKind: KindPick, wantIssue: 40, wantPriority: "P2",
		},
		{
			name: "the configured order ranks; a value outside it ranks last",
			pick: config.Pick{Shape: config.PickProject, Project: &config.Project{Number: 1, PriorityField: "Priority", Order: []string{"Urgent", "Later"}}},
			queue: []github.QueueIssue{
				issue(40, "urgent", "Urgent", "2026-03-01"),
				issue(50, "later", "Later", "2026-01-01"),
				issue(60, "unknown", "Whatever", "2025-01-01"),
			},
			wantKind: KindPick, wantIssue: 40, wantPriority: "Urgent",
		},
		{
			name:     "an issue outside the configured project is invisible",
			queue:    []github.QueueIssue{issue(30, "in-project-9", "P0", "2026-01-01", offProject())},
			wantKind: KindIdle,
		},
		{
			name:     "project membership is the configured project number",
			pick:     config.Pick{Shape: config.PickProject, Project: &config.Project{Number: 9, PriorityField: "Priority", Order: []string{"P0", "P1", "P2"}}},
			queue:    []github.QueueIssue{issue(30, "in-project-9", "P0", "2026-01-01", offProject())},
			wantKind: KindPick, wantIssue: 30, wantPriority: "P0",
		},
		// assignees and partial responses
		{
			name: "foreign assignee skipped; daemon-assigned issue eligible",
			queue: []github.QueueIssue{
				issue(10, "human-claimed", "P0", "2026-01-01", assignees("a-human")),
				issue(20, "daemon-claimed", "P0", "2026-02-01", assignees(login)),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P0",
		},
		{
			name:     "only human-claimed candidates: verdict idle",
			queue:    []github.QueueIssue{issue(10, "human-claimed", "P0", "2026-01-01", assignees("a-human", "other"))},
			wantKind: KindIdle,
		},
		{
			name:  "empty daemon login: assigned issues skipped, unassigned picked",
			login: "-",
			queue: []github.QueueIssue{
				issue(10, "assigned", "P0", "2026-01-01", assignees(login)),
				issue(20, "unassigned", "P1", "2026-02-01"),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
		},
		{
			name: "human + daemon co-assignees: skipped",
			queue: []github.QueueIssue{
				issue(10, "human-and-daemon", "P0", "2026-01-01", assignees("a-human", login)),
				issue(20, "unassigned", "P1", "2026-02-01"),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
		},
		{
			// The bash filter read a missing list as empty and picked #10. An
			// unseen blocker or assignee could be the reason not to, so here
			// the partial ticket is passed over and the filter still survives.
			name: "null blockedBy/assignees: skipped as incomplete, filter survives",
			queue: []github.QueueIssue{
				{Number: 10, Title: "partial-node", CreatedAt: "2026-01-01T00:00:00Z", Labels: []string{LabelAFK}, Incomplete: "blockers and assignees", ProjectItems: []github.QueueProjectItem{{Project: 1, Priority: "P1"}}},
				issue(20, "healthy", "P1", "2026-02-01"),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
			wantSkipped: []string{"GitHub did not return its blockers and assignees"},
		},
		{
			name: "a missing priority ranks as the last value does, even when the order repeats it",
			pick: config.Pick{Shape: config.PickProject, Project: &config.Project{Number: 1, PriorityField: "Priority", Order: []string{"P2", "P1", "P2"}}},
			queue: []github.QueueIssue{
				issue(40, "p1", "P1", "2025-01-01"),
				issue(50, "no-prio", "", "2026-01-01"),
			},
			wantKind: KindPick, wantIssue: 50, wantPriority: "P2",
		},
		{
			name: "blockedBy hasNextPage: candidate treated as blocked",
			queue: []github.QueueIssue{
				issue(10, "over-50-blockers", "P0", "2026-01-01", blockedBy("CLOSED"), truncatedBlockers()),
				issue(20, "clean-p1", "P1", "2026-02-01"),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
		},
		{
			name: "state labels exclude a candidate",
			queue: []github.QueueIssue{
				issue(10, "in-progress", "P0", "2026-01-01", labels(LabelInProgress)),
				issue(11, "done", "P0", "2026-01-01", labels(LabelDone)),
				issue(12, "failed", "P0", "2026-01-01", labels(LabelFailed)),
				issue(13, "paused", "P0", "2026-01-01", labels(LabelPaused)),
				issue(20, "clean", "P1", "2026-02-01"),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
		},
		// wayfinder routing
		{
			name:     "wayfinder:research pick: verdict pick-decision, type research",
			queue:    []github.QueueIssue{issue(10, "Which merge gate?", "P1", "2026-01-01", labels("wayfinder:research"))},
			wantKind: KindPickDecision, wantIssue: 10, wantPriority: "P1", wantType: "research",
		},
		{
			name:     "wayfinder:task pick: type task",
			queue:    []github.QueueIssue{issue(11, "Provision the key", "P1", "2026-01-01", labels("wayfinder:task"))},
			wantKind: KindPickDecision, wantIssue: 11, wantPriority: "P1", wantType: "task",
		},
		{
			name:     "plain AFK Slice: verdict pick, no type",
			queue:    []github.QueueIssue{issue(20, "Tracer", "P1", "2026-02-01")},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
		},
		{
			name: "wayfinder:grilling candidate skipped with a note",
			queue: []github.QueueIssue{
				issue(10, "grill the shape", "P0", "2026-01-01", labels("wayfinder:grilling")),
				issue(20, "clean slice", "P1", "2026-02-01"),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
			wantSkipped: []string{"wayfinder:grilling is not an AFK ticket type"},
		},
		{
			name: "multi-type wayfinder candidate skipped as ambiguous (wayfinder:research,wayfinder:grilling)",
			queue: []github.QueueIssue{
				issue(10, "both types", "P0", "2026-01-01", labels("wayfinder:research", "wayfinder:grilling")),
				issue(20, "clean slice", "P1", "2026-02-01"),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
			wantSkipped: []string{"ambiguous wayfinder labels (wayfinder:grilling,wayfinder:research)"},
		},
		{
			name: "multi-type wayfinder candidate skipped as ambiguous (wayfinder:grilling,wayfinder:research)",
			queue: []github.QueueIssue{
				issue(10, "both types", "P0", "2026-01-01", labels("wayfinder:grilling", "wayfinder:research")),
				issue(20, "clean slice", "P1", "2026-02-01"),
			},
			wantKind: KindPick, wantIssue: 20, wantPriority: "P1",
			wantSkipped: []string{"ambiguous wayfinder labels (wayfinder:grilling,wayfinder:research)"},
		},
		// label-only pick
		{
			name: "oldest open unblocked AFK issue with no human assignee is picked, priority null",
			pick: labelsPick,
			queue: []github.QueueIssue{
				labelIssue(10, "newer", "2026-03-01"),
				labelIssue(20, "oldest-but-blocked", "2026-01-01", blockedBy("OPEN")),
				labelIssue(30, "oldest-but-human", "2026-01-02", assignees("a-human")),
				labelIssue(40, "oldest-eligible", "2026-02-01", blockedBy("CLOSED"), assignees(login)),
			},
			wantKind: KindPick, wantIssue: 40,
		},
		{
			name: "label-only: state labels exclude, wayfinder routing applies, oldest first",
			pick: labelsPick,
			queue: []github.QueueIssue{
				labelIssue(10, "done", "2026-01-01", labels(LabelDone)),
				labelIssue(11, "wf", "2026-01-02", labels("wayfinder:research")),
				labelIssue(12, "slice", "2026-01-03"),
			},
			wantKind: KindPickDecision, wantIssue: 11, wantType: "research",
		},
		{
			name:     "empty queue: verdict idle, exit 0",
			pick:     labelsPick,
			wantKind: KindIdle,
		},
		// beyond the bash suite
		{
			name: "candidates of one rank and one age keep the order GitHub gave",
			pick: labelsPick,
			queue: []github.QueueIssue{
				labelIssue(31, "first", "2026-01-01"),
				labelIssue(30, "second", "2026-01-01"),
			},
			wantKind: KindPick, wantIssue: 31,
		},
		{
			name:     "a project pick with no project items at all is idle",
			queue:    []github.QueueIssue{labelIssue(10, "no items", "2026-01-01")},
			wantKind: KindIdle,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := Facts{Login: login, Pick: projectPick, Queue: tt.queue}
			if tt.pick.Shape != "" {
				facts.Pick = tt.pick
			}
			if tt.login == "-" {
				facts.Login = ""
			}
			before := snapshot(facts)
			got := Derive(facts)
			if got.Kind != tt.wantKind {
				t.Fatalf("Kind = %q, want %q (%+v)", got.Kind, tt.wantKind, got)
			}
			if tt.wantIssue == 0 {
				if got.Pick != nil || got.Next.Action != ActionNone {
					t.Errorf("Pick = %+v, Next = %+v; want none", got.Pick, got.Next)
				}
			} else if got.Pick == nil || got.Pick.Issue != tt.wantIssue || got.Pick.Priority != tt.wantPriority || got.Pick.Type != tt.wantType {
				t.Errorf("Pick = %+v, want issue %d priority %q type %q", got.Pick, tt.wantIssue, tt.wantPriority, tt.wantType)
			}
			if len(got.Skipped) != len(tt.wantSkipped) {
				t.Fatalf("Skipped = %+v, want %d", got.Skipped, len(tt.wantSkipped))
			}
			for i, want := range tt.wantSkipped {
				if got.Skipped[i].Issue != 10 || !strings.Contains(got.Skipped[i].Reason, want) {
					t.Errorf("Skipped[%d] = %+v, want issue 10 and %q", i, got.Skipped[i], want)
				}
			}
			if after := snapshot(facts); !reflect.DeepEqual(before, after) {
				t.Errorf("Derive changed its input: the queue is now %v", after)
			}
		})
	}
}

// snapshot is the queue's issue numbers in order, to show Derive left it alone.
func snapshot(f Facts) []int {
	var numbers []int
	for _, i := range f.Queue {
		numbers = append(numbers, i.Number)
	}
	return numbers
}

func TestDeriveNext(t *testing.T) {
	t.Run("a Slice's next action is to implement it", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: labelsPick, Queue: []github.QueueIssue{labelIssue(20, "Tracer", "2026-02-01")}})
		want := Next{Action: ActionImplement, Issue: 20}
		if got.Next != want || got.Pick.Title != "Tracer" {
			t.Errorf("Next = %+v, Pick = %+v", got.Next, got.Pick)
		}
	})
	t.Run("a research Decision ticket's next action is to research it", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: labelsPick, Queue: []github.QueueIssue{
			labelIssue(11, "wf", "2026-01-02", labels("wayfinder:research")),
			labelIssue(12, "slice", "2026-01-03"),
		}})
		if want := (Next{Action: ActionResearch, Issue: 11}); got.Kind != KindPickDecision || got.Next != want || got.Pick.Type != "research" {
			t.Errorf("Derive = %+v", got)
		}
	})
	t.Run("a task Decision ticket's next action is to do the task", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: labelsPick, Queue: []github.QueueIssue{
			labelIssue(11, "wf", "2026-01-02", labels("wayfinder:task")),
			labelIssue(12, "slice", "2026-01-03"),
		}})
		if want := (Next{Action: ActionTask, Issue: 11}); got.Kind != KindPickDecision || got.Next != want || got.Pick.Type != "task" {
			t.Errorf("Derive = %+v", got)
		}
	})
	t.Run("every Lane is handled: deployed is no longer in the not-handled list", func(t *testing.T) {
		for _, facts := range []Facts{{Held: []int{1}}, {Pick: labelsPick}, {Pick: labelsPick, Queue: []github.QueueIssue{labelIssue(1, "x", "2026-01-01")}}} {
			got := Derive(facts)
			if got.NotHandled == nil || len(got.NotHandled) != 0 {
				t.Errorf("NotHandled = %q", got.NotHandled)
			}
			got.NotHandled = append(got.NotHandled, "scribbled") // a caller's edit must not reach the next derivation
		}
		if got := Derive(Facts{}); len(got.NotHandled) != 0 {
			t.Errorf("NotHandled = %q", got.NotHandled)
		}
	})
	t.Run("derivation holds no memory between calls", func(t *testing.T) {
		facts := Facts{Login: login, Pick: projectPick, Queue: []github.QueueIssue{
			issue(10, "grill", "P0", "2026-01-01", labels("wayfinder:grilling")),
			issue(20, "clean", "P1", "2026-02-01"),
		}}
		first, second := Derive(facts), Derive(facts)
		if !reflect.DeepEqual(first, second) {
			t.Errorf("two derivations of the same facts differ:\n%+v\n%+v", first, second)
		}
	})
}

func TestOpenAgentPR(t *testing.T) {
	queue := []github.QueueIssue{issue(10, "has a PR", "P0", "2026-01-01"), issue(20, "clean", "P1", "2026-02-01")}
	t.Run("a ticket with an open Agent PR is past the pick: the next one is taken", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: projectPick, Queue: queue, AgentPRs: map[int]int{10: 77}})
		if got.Kind != KindPick || got.Next.Issue != 20 {
			t.Fatalf("state = %s, next = %+v", got.Kind, got.Next)
		}
		if len(got.Skipped) != 1 || got.Skipped[0].Issue != 10 || !strings.Contains(got.Skipped[0].Reason, "Agent PR #77 is open") {
			t.Errorf("skipped = %+v", got.Skipped)
		}
	})
	t.Run("an Agent PR for another ticket excludes nothing", func(t *testing.T) {
		got := Derive(Facts{Login: login, Pick: projectPick, Queue: queue, AgentPRs: map[int]int{100: 78}})
		if got.Next.Issue != 10 || len(got.Skipped) != 0 {
			t.Errorf("next = %+v, skipped = %+v", got.Next, got.Skipped)
		}
	})
}
