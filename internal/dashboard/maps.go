package dashboard

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/benjr70/auto-agent/internal/state"
)

// MapsRefreshEvery is how often the maps are read again. Their query is
// GraphQL, which the ETag cache does not cover, and a Map moves slowly.
const MapsRefreshEvery = 5 * time.Minute

// mapsQuery is the main query a maps refresh makes: the open Maps with their
// sub-issues, and the open AFK tickets for the queue's split. A second query,
// blockerStatesQuery, follows only when a body names a blocker this one did
// not see.
//
// Page sizes are bounded by GitHub's static node budget (the product of
// every first: down each path, summed, must stay under 500,000 or the query
// is rejected with MAX_NODE_LIMIT_EXCEEDED): 20 + 20x30 + 20x30x(5+10+10) +
// 100 + 100x30 = 18,720. Truncation is never silent: totalCount and
// pageInfo flag a partial view.
const mapsQuery = `query($owner: String!, $name: String!, $map: String!, $afk: String!) {
  repository(owner: $owner, name: $name) {
    maps: issues(first: 20, labels: [$map], states: OPEN, orderBy: {field: CREATED_AT, direction: DESC}) {
      totalCount
      pageInfo { hasNextPage }
      nodes {
        number title url body
        subIssues(first: 30) {
          totalCount
          pageInfo { hasNextPage }
          nodes {
            number title url state body
            assignees(first: 5) { nodes { login } }
            labels(first: 10) { nodes { name } }
            blockedBy(first: 10) { nodes { number state } pageInfo { hasNextPage } }
          }
        }
      }
    }
    queue: issues(first: 100, labels: [$afk], states: OPEN) {
      pageInfo { hasNextPage }
      nodes { labels(first: 30) { nodes { name } } }
    }
  }
}`

// mapsData is the maps query's data.
type mapsData struct {
	Repository struct {
		Maps  *connection[mapNode] `json:"maps"`
		Queue *connection[struct {
			Labels names `json:"labels"`
		}] `json:"queue"`
	} `json:"repository"`
	// blockerStates are the states the second query read for body blockers
	// the main query did not see; not part of the main query's data.
	blockerStates map[int]string
}

type connection[T any] struct {
	TotalCount *int `json:"totalCount"`
	PageInfo   struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pageInfo"`
	Nodes []T `json:"nodes"`
}

type mapNode struct {
	Number    int                   `json:"number"`
	Title     string                `json:"title"`
	URL       string                `json:"url"`
	Body      string                `json:"body"`
	SubIssues connection[childNode] `json:"subIssues"`
}

type childNode struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	State     string `json:"state"`
	Body      string `json:"body"`
	Assignees struct {
		Nodes []struct {
			Login string `json:"login"`
		} `json:"nodes"`
	} `json:"assignees"`
	Labels    names `json:"labels"`
	BlockedBy connection[struct {
		Number int    `json:"number"`
		State  string `json:"state"`
	}] `json:"blockedBy"`
}

type names struct {
	Nodes []struct {
		Name string `json:"name"`
	} `json:"nodes"`
}

func (n names) list() []string {
	out := make([]string, len(n.Nodes))
	for i, node := range n.Nodes {
		out[i] = node.Name
	}
	return out
}

// A Map's Destination: a `## Destination` heading (text on the next line)
// or an inline `**Destination**:`. The word must END the label, so prose
// like "Destinations are still being scoped" is not a Destination.
var destinationLine = regexp.MustCompile(`(?i)^(?:#{1,6}\s*)?\*{0,2}Destination\*{0,2}\s*(?::\s*(.*))?$`)

// bodyBlocker is the body-text dependency the slicing skills write:
// `Blocked by #N`.
var bodyBlocker = regexp.MustCompile(`(?i)Blocked by\s+#(\d+)`)

func destination(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		m := destinationLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		value := strings.TrimSpace(m[1])
		if value == "" {
			// The heading form: the sentence is on a following line.
			for _, next := range lines[i+1:] {
				if next = strings.TrimSpace(next); next == "" {
					continue
				}
				if !strings.HasPrefix(next, "#") {
					value = next
				}
				break
			}
		}
		return strings.TrimSpace(strings.ReplaceAll(value, "**", ""))
	}
	return ""
}

// bodyBlockers are the issues a body says it is blocked by, in order, once
// each.
func bodyBlockers(body string) []int {
	numbers := []int{}
	for _, m := range bodyBlocker.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && !slices.Contains(numbers, n) {
			numbers = append(numbers, n)
		}
	}
	return numbers
}

// statesIn are the issue states the payload itself names: each child's, and
// each native blocker's. The first one seen wins.
func statesIn(maps []mapNode) map[int]string {
	states := map[int]string{}
	note := func(number int, state string) {
		if _, seen := states[number]; !seen && state != "" {
			states[number] = state
		}
	}
	for _, m := range maps {
		for _, child := range m.SubIssues.Nodes {
			note(child.Number, child.State)
			for _, b := range child.BlockedBy.Nodes {
				note(b.Number, b.State)
			}
		}
	}
	return states
}

// maxBlockerStates caps the second query's issues, as the Python Dashboard
// does; a blocker past the cap stays unseen and reads as open.
const maxBlockerStates = 80

// unseenBodyBlockers are the body blockers whose state the main query did
// not see, ascending, at most maxBlockerStates of them.
func unseenBodyBlockers(maps []mapNode) []int {
	known := statesIn(maps)
	wanted := []int{}
	for _, m := range maps {
		for _, child := range m.SubIssues.Nodes {
			for _, n := range bodyBlockers(child.Body) {
				if _, seen := known[n]; !seen && !slices.Contains(wanted, n) {
					wanted = append(wanted, n)
				}
			}
		}
	}
	slices.Sort(wanted)
	if len(wanted) > maxBlockerStates {
		wanted = wanted[:maxBlockerStates]
	}
	return wanted
}

// blockerStatesQuery is the second query: the state of each numbered issue,
// one alias apiece. Its node cost is one per issue.
func blockerStatesQuery(numbers []int) string {
	fields := make([]string, len(numbers))
	for i, n := range numbers {
		fields[i] = fmt.Sprintf("i%d: issue(number: %d) { number state }", n, n)
	}
	return "query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { " +
		strings.Join(fields, " ") + " } }"
}

// blockerStatesData is the second query's data.
type blockerStatesData struct {
	Repository map[string]*struct {
		Number int    `json:"number"`
		State  string `json:"state"`
	} `json:"repository"`
}

func (d blockerStatesData) states() map[int]string {
	states := map[int]string{}
	for _, issue := range d.Repository {
		if issue != nil && issue.Number != 0 && issue.State != "" {
			states[issue.Number] = issue.State
		}
	}
	return states
}

// ready is whether a Map's child is on its frontier: open, unassigned and
// blocked by no open issue, natively or in its body. A blocker whose state
// neither query saw reads as open, and so does a native page cut off,
// so the card hides work rather than calling blocked work ready.
func ready(child childNode, states map[int]string) bool {
	if child.State != "OPEN" || len(child.Assignees.Nodes) > 0 || child.BlockedBy.PageInfo.HasNextPage {
		return false
	}
	for _, b := range child.BlockedBy.Nodes {
		if b.State == "OPEN" {
			return false
		}
	}
	for _, n := range bodyBlockers(child.Body) {
		if state, seen := states[n]; !seen || state == "OPEN" {
			return false
		}
	}
	return true
}

// shapeMaps is the maps section of a payload: each open Map with its
// frontier.
func shapeMaps(data mapsData) Maps {
	shaped := Maps{Items: []MapItem{}}
	conn := data.Repository.Maps
	if conn == nil {
		return shaped
	}
	states := statesIn(conn.Nodes)
	for n, st := range data.blockerStates {
		if _, seen := states[n]; !seen {
			states[n] = st
		}
	}
	for _, m := range conn.Nodes {
		item := MapItem{Number: m.Number, Title: m.Title, URL: textOrNull(m.URL), Destination: destination(m.Body), Frontier: []FrontierItem{}}
		for _, child := range m.SubIssues.Nodes {
			if !ready(child, states) {
				continue
			}
			labels := child.Labels.list()
			ticket := FrontierItem{Number: child.Number, Title: child.Title, URL: textOrNull(child.URL), Type: textOrNull(state.WayfinderType(labels))}
			switch {
			case slices.Contains(labels, state.LabelAFK):
				ticket.Badge = textOrNull(state.LabelAFK)
			case slices.Contains(labels, state.LabelHITL):
				ticket.Badge = textOrNull(state.LabelHITL)
			}
			item.Frontier = append(item.Frontier, ticket)
		}
		subs := m.SubIssues
		item.Partial = subs.PageInfo.HasNextPage || (subs.TotalCount != nil && *subs.TotalCount > len(subs.Nodes))
		shaped.Truncated = shaped.Truncated || item.Partial
		shaped.Items = append(shaped.Items, item)
	}
	total := len(shaped.Items)
	if conn.TotalCount != nil {
		total = *conn.TotalCount
	}
	shaped.Total = &total
	shaped.Truncated = shaped.Truncated || conn.PageInfo.HasNextPage
	return shaped
}

// mapsRead is one good read of the maps query.
type mapsRead struct {
	maps Maps
	// slices and wayfinder split the queue's untaken AFK tickets; nil when
	// GitHub left the queue out.
	slices, wayfinder *int
	// queueCut is set when the queue had more tickets than the query takes.
	queueCut bool
}

func readMapsData(data mapsData) mapsRead {
	read := mapsRead{maps: shapeMaps(data)}
	if queue := data.Repository.Queue; queue != nil {
		var slices, wayfinder int
		for _, ticket := range queue.Nodes {
			switch labels := ticket.Labels.list(); {
			case state.Taken(labels):
			case state.WayfinderType(labels) != "":
				wayfinder++
			default:
				slices++
			}
		}
		read.slices, read.wayfinder, read.queueCut = &slices, &wayfinder, queue.PageInfo.HasNextPage
	}
	return read
}

func (s *Server) readMaps(ctx context.Context) (mapsRead, error) {
	owner, name, _ := strings.Cut(s.Host.Repo, "/")
	var data mapsData
	variables := map[string]any{"owner": owner, "name": name, "map": state.LabelMap, "afk": state.LabelAFK}
	if err := s.GitHub.GraphQL(ctx, mapsQuery, variables, &data); err != nil {
		return mapsRead{}, fmt.Errorf("reading the maps: %w", err)
	}
	if data.Repository.Maps == nil {
		// Data came back, but not the maps: that is not "no maps".
		return mapsRead{}, fmt.Errorf("reading the maps: GitHub returned no maps of %s", s.Host.Repo)
	}
	if unseen := unseenBodyBlockers(data.Repository.Maps.Nodes); len(unseen) > 0 {
		// The states of body blockers the main query did not see. A failed
		// read keeps what came back (a number that is a pull request, say,
		// fails alone) and leaves the rest unseen, which reads as open: the
		// card hides work rather than calling blocked work ready.
		var blockers blockerStatesData
		_ = s.GitHub.GraphQL(ctx, blockerStatesQuery(unseen), map[string]any{"owner": owner, "name": name}, &blockers)
		data.blockerStates = blockers.states()
	}
	return readMapsData(data), nil
}

// wayfinder is the Wayfinder tile: its counts, every one null when it
// cannot be read. Unknown is not zero: a stale read does not vouch for its
// counts, and a queue GitHub left out is unknown.
func wayfinder(read *mapsRead, section Section) Wayfinder {
	if read == nil || section.Stale {
		return Wayfinder{Unknown: true}
	}
	frontier := 0
	for _, m := range read.maps.Items {
		frontier += len(m.Frontier)
	}
	return Wayfinder{
		Maps: read.maps.Total, Frontier: &frontier, AFK: read.slices,
		Unknown:     read.slices == nil,
		Truncated:   read.maps.Truncated || read.queueCut,
		QueueSlices: read.slices, QueueWayfinder: read.wayfinder,
	}
}

// mapsSection is the maps section and the Wayfinder tile, from one read.
func (s *Server) mapsSection(ctx context.Context, now time.Time) (Maps, Wayfinder) {
	if s.GitHub == nil {
		return Maps{Items: []MapItem{}}, Wayfinder{Unknown: true}
	}
	read, section := s.maps.read(ctx, now, MapsRefreshEvery, s.readMaps)
	maps := Maps{Items: []MapItem{}}
	if read != nil {
		maps = read.maps
	}
	maps.Section = section
	return maps, wayfinder(read, section)
}
