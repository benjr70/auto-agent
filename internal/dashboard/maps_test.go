package dashboard

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

// mapsPayload is server.test.py's MAPS_PAYLOAD, as the maps query's data:
// one Map with a ready research ticket, a ready grilling ticket whose
// blocker closed, a blocked ticket, a claimed one and a closed one.
const mapsPayload = `{"repository": {"maps": {"totalCount": 1, "pageInfo": {"hasNextPage": false}, "nodes": [{
  "number": 575,
  "title": "Wayfinder: adopt AFK planning front-end",
  "url": "https://github.com/o/r/issues/575",
  "body": "## Destination\n\nReached: a spec ready for /to-tickets.\n\n## Notes\n",
  "subIssues": {"totalCount": 5, "pageInfo": {"hasNextPage": false}, "nodes": [
    {"number": 590, "title": "Research: rotate VAPID keys", "url": "https://github.com/o/r/issues/590",
     "state": "OPEN", "body": "", "assignees": {"nodes": []},
     "labels": {"nodes": [{"name": "AFK"}, {"name": "wayfinder:research"}]},
     "blockedBy": {"nodes": []}},
    {"number": 591, "title": "Decide: theming tokens", "url": "https://github.com/o/r/issues/591",
     "state": "OPEN", "body": "", "assignees": {"nodes": []},
     "labels": {"nodes": [{"name": "HITL"}, {"name": "wayfinder:grilling"}]},
     "blockedBy": {"nodes": [{"number": 576, "state": "CLOSED"}]}},
    {"number": 592, "title": "Blocked ticket", "url": "https://github.com/o/r/issues/592",
     "state": "OPEN", "body": "", "assignees": {"nodes": []}, "labels": {"nodes": [{"name": "AFK"}]},
     "blockedBy": {"nodes": [{"number": 590, "state": "OPEN"}]}},
    {"number": 593, "title": "Claimed ticket", "url": "https://github.com/o/r/issues/593",
     "state": "OPEN", "body": "", "assignees": {"nodes": [{"login": "someone"}]},
     "labels": {"nodes": [{"name": "AFK"}]}, "blockedBy": {"nodes": []}},
    {"number": 576, "title": "Done ticket", "url": "https://github.com/o/r/issues/576",
     "state": "CLOSED", "body": "", "assignees": {"nodes": []}, "labels": {"nodes": [{"name": "AFK"}]},
     "blockedBy": {"nodes": []}}
  ]}
}]},
"queue": {"totalCount": 4, "pageInfo": {"hasNextPage": false}, "nodes": [
  {"labels": {"nodes": [{"name": "AFK"}]}},
  {"labels": {"nodes": [{"name": "AFK"}, {"name": "wayfinder:research"}]}},
  {"labels": {"nodes": [{"name": "AFK"}]}},
  {"labels": {"nodes": [{"name": "AFK"}, {"name": "AFK:in-progress"}]}}
]}}}`

func decodeMaps(t *testing.T, payload string) mapsData {
	t.Helper()
	var data mapsData
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// child is one sub-issue: open, unassigned, AFK, unblocked, unless kw says
// otherwise.
func child(number int, kw map[string]any) map[string]any {
	node := map[string]any{
		"number": number, "title": fmt.Sprintf("Ticket %d", number), "url": fmt.Sprintf("https://github.com/o/r/issues/%d", number),
		"state": "OPEN", "body": "", "assignees": map[string]any{"nodes": []any{}},
		"labels": map[string]any{"nodes": []any{map[string]any{"name": "AFK"}}}, "blockedBy": map[string]any{"nodes": []any{}},
	}
	for k, v := range kw {
		node[k] = v
	}
	return node
}

// onePayload is a payload of one Map whose sub-issues are children, with
// subs setting the connection's totalCount and pageInfo.
func onePayload(t *testing.T, body string, children []map[string]any, subs map[string]any) mapsData {
	t.Helper()
	sub := map[string]any{"nodes": children}
	for k, v := range subs {
		sub[k] = v
	}
	data, _ := json.Marshal(map[string]any{"repository": map[string]any{"maps": map[string]any{"nodes": []any{
		map[string]any{"number": 1, "title": "Map", "url": "u", "body": body, "subIssues": sub}}}}})
	return decodeMaps(t, string(data))
}

func frontierOf(m MapItem) []int {
	numbers := []int{}
	for _, ticket := range m.Frontier {
		numbers = append(numbers, ticket.Number)
	}
	return numbers
}

func TestMapsShaping(t *testing.T) {
	t.Run("test_title_destination_and_frontier", func(t *testing.T) {
		m := shapeMaps(decodeMaps(t, mapsPayload)).Items[0]
		if m.Number != 575 || m.Title != "Wayfinder: adopt AFK planning front-end" || *m.URL != "https://github.com/o/r/issues/575" ||
			m.Destination != "Reached: a spec ready for /to-tickets." {
			t.Errorf("map = %+v", m)
		}
		// Of three open children past the closed one, one is blocked and one
		// claimed: the frontier is the ready two, the grilling ticket's
		// blocker being closed.
		if got := frontierOf(m); !reflect.DeepEqual(got, []int{590, 591}) {
			t.Fatalf("frontier = %v", got)
		}
		research, grilling := m.Frontier[0], m.Frontier[1]
		if *research.Type != "research" || *research.Badge != "AFK" || research.Title != "Research: rotate VAPID keys" {
			t.Errorf("research = %+v", research)
		}
		if *grilling.Type != "grilling" || *grilling.Badge != "HITL" {
			t.Errorf("grilling = %+v", grilling)
		}
	})

	t.Run("a ticket with neither badge nor type says null", func(t *testing.T) {
		bare := child(7, map[string]any{"labels": map[string]any{"nodes": []any{}}})
		ticket := shapeMaps(onePayload(t, "", []map[string]any{bare}, nil)).Items[0].Frontier[0]
		if ticket.Type != nil || ticket.Badge != nil {
			t.Errorf("ticket = %+v", ticket)
		}
	})

	t.Run("test_destination_forms", func(t *testing.T) {
		dest := func(body string) string {
			return shapeMaps(onePayload(t, body, []map[string]any{}, nil)).Items[0].Destination
		}
		for body, want := range map[string]string{
			"## Destination\n\nReached: a spec ready for /to-tickets.\n": "Reached: a spec ready for /to-tickets.",
			"**Destination**: ship the thing\n":                          "ship the thing",
			"### **Destination**\nthe **bold** goal\n":                   "the bold goal",
			"destination: lower case\n":                                  "lower case",
			"Destinations are still being scoped\n":                      "",
			"## Destination\n\n## Frontier\n- #2\n":                      "",
			"## Notes\n- nothing\n":                                      "",
			"":                                                           "",
		} {
			if got := dest(body); got != want {
				t.Errorf("destination of %q = %q, want %q", body, got, want)
			}
		}
	})

	t.Run("test_body_blockers", func(t *testing.T) {
		frontier := func(children ...map[string]any) []int {
			return frontierOf(shapeMaps(onePayload(t, "", children, nil)).Items[0])
		}
		blocked := child(561, map[string]any{"body": "Blocked by #559\n"})
		if got := frontier(child(559, nil), blocked); !reflect.DeepEqual(got, []int{559}) {
			t.Errorf("an open sibling blocker: frontier = %v", got)
		}
		if got := frontier(child(559, map[string]any{"state": "CLOSED"}), blocked); !reflect.DeepEqual(got, []int{561}) {
			t.Errorf("a closed sibling blocker: frontier = %v", got)
		}
		// A native edge names its blocker's state: it settles a body blocker
		// of another ticket too.
		native := child(562, map[string]any{"blockedBy": map[string]any{"nodes": []any{map[string]any{"number": 999, "state": "CLOSED"}}}})
		if got := frontier(native, child(561, map[string]any{"body": "Blocked by #999\n"})); !reflect.DeepEqual(got, []int{562, 561}) {
			t.Errorf("a blocker closed by a native edge: frontier = %v", got)
		}
		// A blocker outside the payload is settled by the second query's
		// states; one neither query saw reads as open: the card hides work
		// rather than calling blocked work ready.
		outside := onePayload(t, "", []map[string]any{child(561, map[string]any{"body": "Blocked by #999\n"})}, nil)
		outside.blockerStates = map[int]string{999: "CLOSED"}
		if got := frontierOf(shapeMaps(outside).Items[0]); !reflect.DeepEqual(got, []int{561}) {
			t.Errorf("an outside blocker read closed: frontier = %v", got)
		}
		if got := frontier(child(561, map[string]any{"body": "Blocked by #999\n"})); !reflect.DeepEqual(got, []int{}) {
			t.Errorf("an unseen blocker: frontier = %v", got)
		}
		unseen := onePayload(t, "", []map[string]any{child(559, map[string]any{"state": "CLOSED"}),
			child(561, map[string]any{"body": "Blocked by #559 and Blocked by #999\nBlocked by #998\n"})}, nil)
		if got := unseenBodyBlockers(unseen.Repository.Maps.Nodes); !reflect.DeepEqual(got, []int{998, 999}) {
			t.Errorf("unseenBodyBlockers = %v", got)
		}
		if got := bodyBlockers("Blocked by #12\nBlocked by  #7\nblocked by #12"); !reflect.DeepEqual(got, []int{12, 7}) {
			t.Errorf("bodyBlockers = %v", got)
		}
	})

	t.Run("native blockedBy edges block, and a cut-off page of them too", func(t *testing.T) {
		open := child(3, map[string]any{"blockedBy": map[string]any{"nodes": []any{map[string]any{"number": 9, "state": "OPEN"}}}})
		cut := child(4, map[string]any{"blockedBy": map[string]any{
			"nodes": []any{map[string]any{"number": 9, "state": "CLOSED"}}, "pageInfo": map[string]any{"hasNextPage": true}}})
		if got := frontierOf(shapeMaps(onePayload(t, "", []map[string]any{open, cut, child(5, nil)}, nil)).Items[0]); !reflect.DeepEqual(got, []int{5}) {
			t.Errorf("frontier = %v", got)
		}
	})

	t.Run("test_truncation_is_never_silent", func(t *testing.T) {
		data := onePayload(t, "", []map[string]any{child(2, nil)}, map[string]any{"totalCount": 99, "pageInfo": map[string]any{"hasNextPage": true}})
		data.Repository.Maps.TotalCount = ptr(42)
		shaped := shapeMaps(data)
		if *shaped.Total != 42 || !shaped.Items[0].Partial || !shaped.Truncated {
			t.Errorf("total %v, partial %v, truncated %v", *shaped.Total, shaped.Items[0].Partial, shaped.Truncated)
		}
		// More children than came back is a cut-off list even without a
		// next page.
		short := shapeMaps(onePayload(t, "", []map[string]any{child(2, nil)}, map[string]any{"totalCount": 3}))
		if !short.Items[0].Partial || !short.Truncated {
			t.Errorf("a short page: %+v", short)
		}
		maps := decodeMaps(t, mapsPayload)
		maps.Repository.Maps.PageInfo.HasNextPage = true
		if shaped := shapeMaps(maps); !shaped.Truncated || shaped.Items[0].Partial {
			t.Errorf("more maps than the query takes: %+v", shaped)
		}
		whole := shapeMaps(decodeMaps(t, mapsPayload))
		if whole.Truncated || whole.Items[0].Partial || *whole.Total != 1 {
			t.Errorf("a whole read: %+v", whole)
		}
	})
}

func TestWayfinderTile(t *testing.T) {
	read := readMapsData(decodeMaps(t, mapsPayload))

	t.Run("the counts", func(t *testing.T) {
		// One map, a frontier of two, and of the queue's three untaken
		// tickets two Slices and one Wayfinder ticket.
		got := wayfinder(&read, Section{})
		want := Wayfinder{Maps: ptr(1), Frontier: ptr(2), AFK: ptr(2), QueueSlices: ptr(2), QueueWayfinder: ptr(1)}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tile = %s, want %s", asText(got), asText(want))
		}
	})

	t.Run("test_wayfinder_tile_unknown_is_not_zero", func(t *testing.T) {
		unknown := Wayfinder{Unknown: true}
		if got := wayfinder(nil, Section{Stale: true, Error: ptr("GitHub answered 500")}); !reflect.DeepEqual(got, unknown) {
			t.Errorf("never read: %s", asText(got))
		}
		// A failed refresh keeps the section's last good maps, flagged; the
		// tile does not vouch for them. Python kept maps and afk here, read
		// from the work probe's fresh scan; in Go every count is this one
		// query's, so a failed refresh leaves none fresh.
		if got := wayfinder(&read, Section{Stale: true, Error: ptr("GitHub answered 500")}); !reflect.DeepEqual(got, unknown) {
			t.Errorf("stale: %s", asText(got))
		}
		// A queue GitHub left out is unknown, the maps still counted.
		noQueue := decodeMaps(t, mapsPayload)
		noQueue.Repository.Queue = nil
		r := readMapsData(noQueue)
		got := wayfinder(&r, Section{})
		if !got.Unknown || got.AFK != nil || got.QueueSlices != nil || got.QueueWayfinder != nil || *got.Frontier != 2 {
			t.Errorf("no queue: %s", asText(got))
		}
	})

	t.Run("a cut-off queue says so", func(t *testing.T) {
		cut := decodeMaps(t, mapsPayload)
		cut.Repository.Queue.PageInfo.HasNextPage = true
		r := readMapsData(cut)
		if got := wayfinder(&r, Section{}); !got.Truncated || *got.QueueSlices != 2 {
			t.Errorf("tile = %s", asText(got))
		}
	})
}

func asText(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// queryNodes is GitHub's static node cost of a query: the product of every
// first: down each path, summed. A first: applies to the next { that opens a
// selection set; a { inside an argument list (an input object such as
// orderBy: {...}) is skipped, so it neither takes nor resets the multiplier.
func queryNodes(query string) int {
	stack, pending, total, parens := []int{1}, 0, 0, 0
	for _, token := range regexp.MustCompile(`first:\s*(\d+)|[{}()]`).FindAllStringSubmatch(query, -1) {
		switch {
		case token[1] != "":
			pending, _ = strconv.Atoi(token[1])
		case token[0] == "(":
			parens++
		case token[0] == ")":
			parens--
		case parens > 0:
			// an input-object brace inside an argument list
		case token[0] == "{":
			mult := stack[len(stack)-1]
			if pending != 0 {
				mult *= pending
				total += mult
				pending = 0
			}
			stack = append(stack, mult)
		default:
			stack = stack[:len(stack)-1]
		}
	}
	return total
}

// TestMapsQuery: the product of every first: down each path, summed, must
// stay under the 500,000 nodes GitHub allows a query.
func TestMapsQuery(t *testing.T) {
	t.Run("test_maps_query_is_under_githubs_node_limit", func(t *testing.T) {
		// 20 + 20x30 + 20x30x(5+10+10) + 100 + 100x30, as mapsQuery documents.
		if total := queryNodes(mapsQuery); total != 18_720 || total >= 500_000 {
			t.Errorf("the maps query costs %d nodes, want the documented 18,720", total)
		}
	})
	t.Run("an argument object does not consume the multiplier", func(t *testing.T) {
		query := `{ a: issues(first: 20, orderBy: {field: CREATED_AT, direction: DESC}) {
		  nodes { subIssues(first: 30) { nodes { labels(first: 10) { nodes { name } } } } } } }`
		if total := queryNodes(query); total != 20+20*30+20*30*10 {
			t.Errorf("queryNodes = %d, want %d", total, 20+20*30+20*30*10)
		}
	})
}

// mapsReply is the maps query's answer in a fake GitHub.
func mapsReply() githubtest.Reply {
	return githubtest.Text(200, `{"data": `+mapsPayload+`}`)
}

func TestTheMapsAreOneQueryOfTheConfigRepo(t *testing.T) {
	gh := fakeGitHub(t, openPulls)
	_, ts, c := live(t, stateDir(t), &fakeGate{verdict: loginVerdict}, gh)
	st := status(t, ts)
	maps := st["maps"].(map[string]any)
	if maps["stale"] != false || maps["asOf"] == nil || maps["total"] != 1.0 || maps["truncated"] != false {
		t.Errorf("maps = %v", maps)
	}
	status(t, ts)
	c.add(MapsRefreshEvery)
	status(t, ts)
	var queries []string
	for _, r := range gh.Requests() {
		if r.Path == "/graphql" {
			queries = append(queries, r.Body)
		}
	}
	if len(queries) != 2 {
		t.Fatalf("%d maps queries in two refreshes", len(queries))
	}
	for _, body := range queries {
		var q struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.Unmarshal([]byte(body), &q)
		want := map[string]any{"owner": "acme", "name": "widgets", "map": "wayfinder:map", "afk": "AFK"}
		if q.Query != mapsQuery || !reflect.DeepEqual(q.Variables, want) {
			t.Errorf("variables = %v", q.Variables)
		}
	}
}

func TestTheMapsDegradeOnTheirOwn(t *testing.T) {
	gh := fakeGitHub(t, openPulls)
	gh.HandleGraphQL("subIssues", mapsReply(), githubtest.Text(502, `{"message":"bad gateway"}`))
	_, ts, c := live(t, stateDir(t), &fakeGate{verdict: loginVerdict}, gh)
	status(t, ts)
	c.add(MapsRefreshEvery)
	st := status(t, ts)
	maps := st["maps"].(map[string]any)
	if maps["stale"] != true || maps["error"] == nil || len(maps["items"].([]any)) != 1 {
		t.Errorf("maps = %v", maps)
	}
	if w := st["wayfinder"].(map[string]any); w["unknown"] != true || w["frontier"] != nil || w["maps"] != nil {
		t.Errorf("wayfinder = %v", w)
	}
	// The other sections never noticed.
	if st["openPrs"].(map[string]any)["stale"] != false {
		t.Error("the open PRs went stale with the maps")
	}
	if !strings.Contains(fmt.Sprint(maps["error"]), "502") {
		t.Errorf("error = %v", maps["error"])
	}
}

// blockedPayload is a Map of one ticket blocked in its body by #999, an
// issue outside every Map.
const blockedPayload = `{"repository": {"maps": {"totalCount": 1, "pageInfo": {"hasNextPage": false}, "nodes": [{
  "number": 1, "title": "Map", "url": "u", "body": "",
  "subIssues": {"totalCount": 1, "pageInfo": {"hasNextPage": false}, "nodes": [
    {"number": 561, "title": "Ticket", "url": "u561", "state": "OPEN", "body": "Blocked by #999\n",
     "assignees": {"nodes": []}, "labels": {"nodes": [{"name": "AFK"}]}, "blockedBy": {"nodes": []}}
  ]}
}]}, "queue": {"pageInfo": {"hasNextPage": false}, "nodes": []}}}`

func TestUnseenBodyBlockersAreReadInASecondQuery(t *testing.T) {
	frontier := func(t *testing.T, states githubtest.Reply) ([]any, []string) {
		gh := fakeGitHub(t, openPulls)
		gh.HandleGraphQL("subIssues", githubtest.Text(200, `{"data": `+blockedPayload+`}`))
		gh.HandleGraphQL("i999:", states)
		_, ts, _ := live(t, stateDir(t), &fakeGate{verdict: loginVerdict}, gh)
		maps := status(t, ts)["maps"].(map[string]any)
		if maps["stale"] != false {
			t.Fatalf("maps = %v", maps)
		}
		var queries []string
		for _, r := range gh.Requests() {
			if r.Path == "/graphql" {
				var q struct {
					Query string `json:"query"`
				}
				json.Unmarshal([]byte(r.Body), &q)
				queries = append(queries, q.Query)
			}
		}
		return maps["items"].([]any)[0].(map[string]any)["frontier"].([]any), queries
	}

	t.Run("a closed one frees its ticket", func(t *testing.T) {
		got, queries := frontier(t, githubtest.Text(200, `{"data": {"repository": {"i999": {"number": 999, "state": "CLOSED"}}}}`))
		if len(got) != 1 || got[0].(map[string]any)["number"] != 561.0 {
			t.Errorf("frontier = %v", got)
		}
		if len(queries) != 2 || queries[1] != blockerStatesQuery([]int{999}) {
			t.Errorf("queries = %q", queries)
		}
	})
	t.Run("an open one keeps it blocked", func(t *testing.T) {
		if got, _ := frontier(t, githubtest.Text(200, `{"data": {"repository": {"i999": {"number": 999, "state": "OPEN"}}}}`)); len(got) != 0 {
			t.Errorf("frontier = %v", got)
		}
	})
	t.Run("a failed read keeps it blocked, the maps still fresh", func(t *testing.T) {
		if got, _ := frontier(t, githubtest.Text(502, `{"message":"bad gateway"}`)); len(got) != 0 {
			t.Errorf("frontier = %v", got)
		}
	})
	t.Run("no unseen blockers, no second query", func(t *testing.T) {
		gh := fakeGitHub(t, openPulls)
		_, ts, _ := live(t, stateDir(t), &fakeGate{verdict: loginVerdict}, gh)
		status(t, ts)
		n := 0
		for _, r := range gh.Requests() {
			if r.Path == "/graphql" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d GraphQL queries for a payload whose blockers it saw", n)
		}
	})
}
