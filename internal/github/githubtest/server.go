// Package githubtest is the fake GitHub every package tests against: an HTTP
// server that serves scripted replies and records each request it receives.
package githubtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// TB is the part of *testing.T the fake uses.
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
}

// Server is a fake GitHub API. Point a client's base URL at URL.
type Server struct {
	// URL is the server's base URL, with no trailing slash.
	URL string

	t      TB
	http   *httptest.Server
	mu     sync.Mutex
	routes map[string]*route
	// graphql routes are tried in the order they were scripted.
	graphql []graphqlRoute
	// matched routes answer a method and any path a pattern matches, when
	// no exact route does; tried in the order they were scripted.
	matched []matchedRoute
	seen    []Request
}

type route struct {
	replies []Reply
	next    int
	// answer, when set, computes the reply instead.
	answer func(Request) Reply
}

func (r *route) take(request Request) Reply {
	if r.answer != nil {
		return r.answer(request)
	}
	reply := r.replies[r.next]
	if r.next < len(r.replies)-1 {
		r.next++
	}
	return reply
}

type matchedRoute struct {
	method  string
	pattern *regexp.Regexp
	*route
}

type graphqlRoute struct {
	contains string
	*route
}

// Request is one request the server received.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   string
}

// Reply is one scripted response.
type Reply struct {
	Status int
	Header http.Header
	Body   string
}

// JSON is a reply whose body is v encoded as JSON.
func JSON(status int, v any) Reply {
	body, err := json.Marshal(v)
	if err != nil {
		panic("githubtest: " + err.Error())
	}
	return Reply{Status: status, Body: string(body)}.WithHeader("Content-Type", "application/json; charset=utf-8")
}

// Text is a reply with a plain body.
func Text(status int, body string) Reply {
	return Reply{Status: status, Body: body}
}

// WithHeader returns the reply with one more header.
func (r Reply) WithHeader(key, value string) Reply {
	header := r.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	header.Set(key, value)
	r.Header = header
	return r
}

// WithETag returns the reply carrying an ETag. A request whose If-None-Match
// names that ETag is then answered 304 with no body, as GitHub does.
func (r Reply) WithETag(etag string) Reply { return r.WithHeader("ETag", etag) }

// New starts a fake GitHub that is closed when the test ends. A request no
// reply was scripted for fails the test.
func New(t TB) *Server {
	s := &Server{t: t, routes: map[string]*route{}}
	s.http = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.http.URL
	t.Cleanup(s.Close)
	return s
}

// Close stops the server.
func (s *Server) Close() { s.http.Close() }

// Handle scripts the replies to one method and path, whatever the query
// string. They are served in turn, and the last one repeats.
func (s *Server) Handle(method, path string, replies ...Reply) {
	if len(replies) == 0 {
		panic("githubtest: Handle needs at least one reply")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = &route{replies: replies}
}

// HandleFunc answers one method and path with whatever answer returns for
// each request, for a fake that keeps state: labels that are put on and
// taken off, comments that are posted and listed. answer must not call back
// into the server.
func (s *Server) HandleFunc(method, path string, answer func(Request) Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = &route{answer: answer}
}

// HandleMatch answers one method and every path the pattern matches with
// whatever answer returns, for a path that carries what a test cannot know
// in advance, such as a commit sha. An exact route wins over it.
func (s *Server) HandleMatch(method string, pattern *regexp.Regexp, answer func(Request) Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matched = append(s.matched, matchedRoute{method: method, pattern: pattern, route: &route{answer: answer}})
}

// HandleGraphQL scripts the replies to the GraphQL documents that contain
// the given text, so that a test with several queries need not depend on
// their order. The first scripted match wins, ahead of a plain
// Handle("POST", "/graphql").
func (s *Server) HandleGraphQL(contains string, replies ...Reply) {
	if len(replies) == 0 {
		panic("githubtest: HandleGraphQL needs at least one reply")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.graphql = append(s.graphql, graphqlRoute{contains: contains, route: &route{replies: replies}})
}

// Requests are the requests received so far, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.seen...)
}

// Writes are the received requests that change something: every method but
// GET and HEAD, less the GraphQL posts that are queries.
func (s *Server) Writes() []Request {
	var writes []Request
	for _, r := range s.Requests() {
		switch {
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
		case r.Method == http.MethodPost && r.Path == "/graphql" && !isMutation(r.Body):
		default:
			writes = append(writes, r)
		}
	}
	return writes
}

var (
	graphqlComment  = regexp.MustCompile(`#[^\n]*`)
	graphqlMutation = regexp.MustCompile(`\bmutation\b`)
)

// graphqlQuery is the document a GraphQL request body carries.
func graphqlQuery(body string) (string, bool) {
	var payload struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "", false
	}
	return payload.Query, true
}

// isMutation errs towards yes: a document that names a mutation anywhere
// outside a comment counts, and so does a body that cannot be read, so that
// no write hides from a test.
func isMutation(body string) bool {
	query, ok := graphqlQuery(body)
	return !ok || graphqlMutation.MatchString(graphqlComment.ReplaceAllString(query, ""))
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	request := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: string(body)}
	s.seen = append(s.seen, request)
	var reply Reply
	rt, scripted := s.routes[r.Method+" "+r.URL.Path]
	for _, m := range s.matched {
		if !scripted && m.method == r.Method && m.pattern.MatchString(r.URL.Path) {
			rt, scripted = m.route, true
		}
	}
	if query, ok := graphqlQuery(string(body)); ok && r.Method == http.MethodPost && r.URL.Path == "/graphql" {
		for _, g := range s.graphql {
			if strings.Contains(query, g.contains) {
				rt, scripted = g.route, true
				break
			}
		}
	}
	if scripted {
		reply = rt.take(request)
	}
	s.mu.Unlock()

	if !scripted {
		s.t.Helper()
		s.t.Errorf("githubtest: no reply scripted for %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	for key, values := range reply.Header {
		w.Header()[key] = values
	}
	if etag := reply.Header.Get("ETag"); etag != "" && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(reply.Status)
	if reply.Status != http.StatusNotModified && reply.Status != http.StatusNoContent {
		io.WriteString(w, reply.Body)
	}
}
