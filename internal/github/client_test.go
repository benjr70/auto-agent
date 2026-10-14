package github

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

var (
	ctx = context.Background()
	now = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
)

func newClient(s *githubtest.Server) *Client {
	return New(Options{BaseURL: s.URL, Token: "t0ken", UserAgent: "auto-agent/test", Now: func() time.Time { return now }})
}

func TestResolveToken(t *testing.T) {
	env := func(pairs ...string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			for i := 0; i < len(pairs); i += 2 {
				if pairs[i] == k {
					return pairs[i+1], true
				}
			}
			return "", false
		}
	}
	gh := func(token string, err error) func() (string, error) {
		return func() (string, error) { return token, err }
	}
	t.Run("GH_TOKEN wins and gh is never asked", func(t *testing.T) {
		got, err := ResolveToken(env("GH_TOKEN", "from-env"), func() (string, error) {
			t.Error("gh was asked although GH_TOKEN is set")
			return "", nil
		})
		if err != nil || got != "from-env" {
			t.Errorf("ResolveToken = %q, %v", got, err)
		}
	})
	t.Run("without GH_TOKEN the token is gh's", func(t *testing.T) {
		for _, lookup := range []func(string) (string, bool){env(), env("GH_TOKEN", ""), env("GH_TOKEN", "  ")} {
			got, err := ResolveToken(lookup, gh("from-gh\n", nil))
			if err != nil || got != "from-gh" {
				t.Errorf("ResolveToken = %q, %v", got, err)
			}
		}
	})
	t.Run("when neither yields one the error says so", func(t *testing.T) {
		for _, source := range []func() (string, error){gh("", nil), gh("", errors.New("gh: not logged in")), gh("leaked", errors.New("exit 1"))} {
			got, err := ResolveToken(env(), source)
			if got != "" || !errors.Is(err, ErrNoToken) || !strings.Contains(err.Error(), "GH_TOKEN") || !strings.Contains(err.Error(), "gh auth token") {
				t.Errorf("ResolveToken = %q, %v", got, err)
			}
		}
	})
}

func TestConditionalRequests(t *testing.T) {
	t.Run("second identical GET sends If-None-Match and returns the cached body on 304", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/issues/7",
			githubtest.JSON(200, map[string]any{"number": 7, "title": "first", "state": "open"}).WithETag(`"abc"`))
		c := newClient(s)
		first, err := c.Issue(ctx, "acme", "widgets", 7)
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.Issue(ctx, "acme", "widgets", 7)
		if err != nil {
			t.Fatal(err)
		}
		reqs := s.Requests()
		if reqs[0].Header.Get("If-None-Match") != "" || reqs[1].Header.Get("If-None-Match") != `"abc"` {
			t.Errorf("If-None-Match = %q then %q", reqs[0].Header.Get("If-None-Match"), reqs[1].Header.Get("If-None-Match"))
		}
		if !reflect.DeepEqual(first, second) || second.Title != "first" {
			t.Errorf("first = %+v, second = %+v", first, second)
		}
	})
	t.Run("a changed resource replaces the cached body and its ETag", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/issues/7",
			githubtest.JSON(200, map[string]any{"number": 7, "title": "first"}).WithETag(`"abc"`),
			githubtest.JSON(200, map[string]any{"number": 7, "title": "second"}).WithETag(`"def"`))
		c := newClient(s)
		c.Issue(ctx, "acme", "widgets", 7)
		second, _ := c.Issue(ctx, "acme", "widgets", 7)
		third, _ := c.Issue(ctx, "acme", "widgets", 7)
		reqs := s.Requests()
		if second.Title != "second" || third.Title != "second" || reqs[2].Header.Get("If-None-Match") != `"def"` {
			t.Errorf("second = %q, third = %q, third If-None-Match = %q", second.Title, third.Title, reqs[2].Header.Get("If-None-Match"))
		}
	})
	t.Run("a 304 GitHub answers is served from the remembered body", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets",
			githubtest.JSON(200, map[string]string{"default_branch": "trunk"}).WithETag(`"r1"`),
			githubtest.Reply{Status: 304})
		c := newClient(s)
		c.Repository(ctx, "acme", "widgets")
		again, err := c.Repository(ctx, "acme", "widgets")
		if err != nil || again.DefaultBranch != "trunk" {
			t.Errorf("Repository on 304 = %+v, %v", again, err)
		}
	})
	t.Run("a 304 with nothing remembered is an error, not an empty answer", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets", githubtest.Reply{Status: 304})
		if _, err := newClient(s).Repository(ctx, "acme", "widgets"); err == nil {
			t.Error("a 304 with no cached body gave no error")
		}
	})
	t.Run("a response without an ETag is never cached, and a write is never conditional", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "machine-user"}))
		s.Handle("POST", "/repos/acme/widgets/issues/7/comments", githubtest.JSON(201, map[string]any{"id": 1}).WithETag(`"w"`))
		c := newClient(s)
		c.Login(ctx)
		c.Login(ctx)
		c.CreateComment(ctx, "acme", "widgets", 7, "hi")
		c.CreateComment(ctx, "acme", "widgets", 7, "hi")
		for _, r := range s.Requests() {
			if r.Header.Get("If-None-Match") != "" {
				t.Errorf("%s %s sent If-None-Match %q", r.Method, r.Path, r.Header.Get("If-None-Match"))
			}
		}
	})
}

func TestRequests(t *testing.T) {
	t.Run("every request carries the token, the API version and a user agent", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "machine-user"}))
		newClient(s).Login(ctx)
		h := s.Requests()[0].Header
		if h.Get("Authorization") != "Bearer t0ken" || h.Get("X-GitHub-Api-Version") != "2022-11-28" ||
			h.Get("Accept") != "application/vnd.github+json" || h.Get("User-Agent") != "auto-agent/test" {
			t.Errorf("headers = %v", h)
		}
	})
	t.Run("the client identifies the token's login", func(t *testing.T) {
		stubGH(t, `echo "gh was run" >&2; exit 1`) // no call may run gh
		s := githubtest.New(t)
		s.Handle("GET", "/user", githubtest.JSON(200, map[string]string{"login": "machine-user"}))
		login, err := newClient(s).Login(ctx)
		if err != nil || login != "machine-user" {
			t.Errorf("Login = %q, %v", login, err)
		}
	})
	t.Run("the typed reads decode what the State needs", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets", githubtest.JSON(200, map[string]any{"default_branch": "trunk", "full_name": "acme/widgets"}))
		s.Handle("GET", "/repos/acme/widgets/issues/7", githubtest.JSON(200, map[string]any{
			"number": 7, "title": "T", "body": "B", "state": "open",
			"labels":    []map[string]string{{"name": "AFK"}, {"name": "AFK:in-progress"}},
			"assignees": []map[string]string{{"login": "machine-user"}},
		}))
		s.Handle("GET", "/repos/acme/widgets/issues/8", githubtest.JSON(200, map[string]any{"number": 8, "pull_request": map[string]string{"url": "x"}}))
		s.Handle("GET", "/repos/acme/widgets/pulls/8", githubtest.JSON(200, map[string]any{
			"number": 8, "title": "PR", "state": "open", "draft": true, "merged": false,
			"head": map[string]string{"ref": "feat/issue-7", "sha": "abc"}, "base": map[string]string{"ref": "trunk"},
		}))
		c := newClient(s)
		repo, err := c.Repository(ctx, "acme", "widgets")
		if err != nil || repo.DefaultBranch != "trunk" {
			t.Errorf("Repository = %+v, %v", repo, err)
		}
		issue, err := c.Issue(ctx, "acme", "widgets", 7)
		want := Issue{Number: 7, Title: "T", Body: "B", State: "open", Labels: []string{"AFK", "AFK:in-progress"}, Assignees: []string{"machine-user"}}
		if err != nil || !reflect.DeepEqual(issue, want) {
			t.Errorf("Issue = %+v, %v", issue, err)
		}
		if pr, _ := c.Issue(ctx, "acme", "widgets", 8); !pr.IsPullRequest {
			t.Errorf("issue 8 is a pull request: %+v", pr)
		}
		pull, err := c.PullRequest(ctx, "acme", "widgets", 8)
		wantPull := PullRequest{Number: 8, Title: "PR", State: "open", Draft: true, HeadRef: "feat/issue-7", HeadSHA: "abc", BaseRef: "trunk", Labels: []string{}, Mergeable: MergeableUnknown}
		if err != nil || !reflect.DeepEqual(pull, wantPull) {
			t.Errorf("PullRequest = %+v, %v", pull, err)
		}
	})
	t.Run("the typed writes send what GitHub expects", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/repos/acme/widgets/issues/7/labels", githubtest.JSON(200, []any{}))
		s.Handle("DELETE", "/repos/acme/widgets/issues/7/labels/AFK:in-progress", githubtest.JSON(200, []any{}))
		s.Handle("POST", "/repos/acme/widgets/issues/7/comments", githubtest.JSON(201, map[string]any{"id": 99}))
		c := newClient(s)
		if err := c.AddLabels(ctx, "acme", "widgets", 7, "AFK:in-progress", "AFK"); err != nil {
			t.Error(err)
		}
		if err := c.RemoveLabel(ctx, "acme", "widgets", 7, "AFK:in-progress"); err != nil {
			t.Error(err)
		}
		if err := c.CreateComment(ctx, "acme", "widgets", 7, "done"); err != nil {
			t.Error(err)
		}
		reqs := s.Requests()
		if reqs[0].Body != `{"labels":["AFK:in-progress","AFK"]}` || reqs[2].Body != `{"body":"done"}` || reqs[0].Header.Get("Content-Type") != "application/json" {
			t.Errorf("bodies = %q, %q; content type %q", reqs[0].Body, reqs[2].Body, reqs[0].Header.Get("Content-Type"))
		}
	})
	t.Run("removing a label the issue does not carry is not an error", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("DELETE", "/repos/acme/widgets/issues/7/labels/AFK", githubtest.JSON(404, map[string]string{"message": "Label does not exist"}))
		if err := newClient(s).RemoveLabel(ctx, "acme", "widgets", 7, "AFK"); err != nil {
			t.Error(err)
		}
	})
	t.Run("an API failure is a typed error with the status and GitHub's message, never the token", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/issues/7", githubtest.JSON(404, map[string]string{"message": "Not Found"}))
		_, err := newClient(s).Issue(ctx, "acme", "widgets", 7)
		var api *APIError
		if !errors.As(err, &api) || api.Status != 404 || api.Message != "Not Found" || !IsNotFound(err) {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(err.Error(), "GET /repos/acme/widgets/issues/7") || strings.Contains(err.Error(), "t0ken") {
			t.Errorf("error text = %q", err.Error())
		}
	})
}

func TestRateLimits(t *testing.T) {
	reset := now.Add(17 * time.Minute)
	rateHeaders := func(r githubtest.Reply, remaining string) githubtest.Reply {
		return r.WithHeader("X-RateLimit-Limit", "5000").WithHeader("X-RateLimit-Remaining", remaining).
			WithHeader("X-RateLimit-Used", "12").WithHeader("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10)).
			WithHeader("X-RateLimit-Resource", "core")
	}
	t.Run("rate-limit headers are surfaced to the caller", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/user", rateHeaders(githubtest.JSON(200, map[string]string{"login": "machine-user"}), "4988"))
		c := newClient(s)
		if _, ok := c.Rate(ResourceCore); ok {
			t.Error("a rate is reported before any response")
		}
		c.Login(ctx)
		got, ok := c.Rate(ResourceCore)
		if _, other := c.Rate(ResourceGraphQL); other {
			t.Error("a core response reported a graphql rate")
		}
		want := Rate{Resource: "core", Limit: 5000, Remaining: 4988, Used: 12, Reset: reset}
		if !ok || !got.Reset.Equal(want.Reset) || got.Limit != want.Limit || got.Remaining != want.Remaining || got.Used != want.Used || got.Resource != want.Resource {
			t.Errorf("Rate = %+v, %v; want %+v", got, ok, want)
		}
	})
	tests := []struct {
		name      string
		reply     githubtest.Reply
		secondary bool
		reset     time.Time
	}{
		{
			name:  "a rate-limit response becomes the typed error with its reset time",
			reply: rateHeaders(githubtest.JSON(403, map[string]string{"message": "API rate limit exceeded for user ID 1."}), "0"),
			reset: reset,
		},
		{
			name:  "a primary limit answered 429 is the same error",
			reply: rateHeaders(githubtest.JSON(429, map[string]string{"message": "API rate limit exceeded"}), "0"),
			reset: reset,
		},
		{
			name:      "a secondary limit carries Retry-After as its reset",
			reply:     rateHeaders(githubtest.JSON(403, map[string]string{"message": "You have exceeded a secondary rate limit."}), "4000").WithHeader("Retry-After", "90"),
			secondary: true,
			reset:     now.Add(90 * time.Second),
		},
		{
			name:      "a secondary limit whose Retry-After is a date resets then",
			reply:     rateHeaders(githubtest.JSON(429, map[string]string{"message": "slow down"}), "4000").WithHeader("Retry-After", now.Add(5*time.Minute).Format(http.TimeFormat)),
			secondary: true,
			reset:     now.Add(5 * time.Minute),
		},
		{
			name:  "a spent quota stays primary even when it carries Retry-After",
			reply: rateHeaders(githubtest.JSON(429, map[string]string{"message": "API rate limit exceeded"}), "0").WithHeader("Retry-After", "30"),
			reset: reset,
		},
		{
			name:  "a spent quota that names no reset waits a minute, never until the zero time",
			reply: githubtest.JSON(403, map[string]string{"message": "API rate limit exceeded"}).WithHeader("X-RateLimit-Remaining", "0"),
			reset: now.Add(time.Minute),
		},
		{
			name:      "the older abuse wording is a secondary limit too",
			reply:     rateHeaders(githubtest.JSON(403, map[string]string{"message": "You have triggered an abuse detection mechanism."}), "4000"),
			secondary: true,
			reset:     now.Add(time.Minute),
		},
		{
			name:      "a secondary limit with no Retry-After waits a minute",
			reply:     rateHeaders(githubtest.JSON(403, map[string]string{"message": "You have exceeded a secondary rate limit. Please wait."}), "4000"),
			secondary: true,
			reset:     now.Add(time.Minute),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := githubtest.New(t)
			s.Handle("GET", "/user", tt.reply)
			_, err := newClient(s).Login(ctx)
			var limited *RateLimitError
			if !errors.As(err, &limited) {
				t.Fatalf("error = %v, want a *RateLimitError", err)
			}
			if limited.Secondary != tt.secondary || !limited.Reset.Equal(tt.reset) {
				t.Errorf("RateLimitError = %+v, want secondary %v reset %v", limited, tt.secondary, tt.reset)
			}
		})
	}
	t.Run("a client with no token fails before any request", func(t *testing.T) {
		s := githubtest.New(t)
		_, err := New(Options{BaseURL: s.URL}).Login(ctx)
		if !errors.Is(err, ErrNoToken) || len(s.Requests()) != 0 {
			t.Errorf("error = %v, requests = %d", err, len(s.Requests()))
		}
	})
	t.Run("a failure that is not GitHub's JSON keeps a short excerpt", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/user", githubtest.Text(502, strings.Repeat("<html>", 500)))
		_, err := newClient(s).Login(ctx)
		var api *APIError
		if !errors.As(err, &api) || api.Status != 502 || len(api.Message) > 400 {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("a 403 that is not a rate limit stays an API error", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/user", rateHeaders(githubtest.JSON(403, map[string]string{"message": "Resource not accessible by personal access token"}), "4000"))
		_, err := newClient(s).Login(ctx)
		var limited *RateLimitError
		var api *APIError
		if errors.As(err, &limited) || !errors.As(err, &api) || api.Status != http.StatusForbidden {
			t.Errorf("error = %v", err)
		}
	})
}

func TestGraphQL(t *testing.T) {
	const query = `query($owner: String!) { repository(owner: $owner, name: "widgets") { id } }`
	type result struct {
		Repository *struct {
			ID string `json:"id"`
		} `json:"repository"`
	}
	t.Run("a query posts to /graphql and decodes the data", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", githubtest.JSON(200, map[string]any{"data": map[string]any{"repository": map[string]string{"id": "R_1"}}}))
		var out result
		if err := newClient(s).GraphQL(ctx, query, map[string]any{"owner": "acme"}, &out); err != nil {
			t.Fatal(err)
		}
		req := s.Requests()[0]
		if out.Repository == nil || out.Repository.ID != "R_1" || !strings.Contains(req.Body, `"variables":{"owner":"acme"}`) || !strings.Contains(req.Body, `"query":"query($owner`) {
			t.Errorf("out = %+v, body = %s", out, req.Body)
		}
	})
	t.Run("partial data is decoded and the errors come back beside it", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", githubtest.JSON(200, map[string]any{
			"data":   map[string]any{"repository": map[string]string{"id": "R_1"}},
			"errors": []map[string]any{{"type": "FORBIDDEN", "message": "Resource not accessible", "path": []any{"repository", "projectV2"}}},
		}))
		var out result
		err := newClient(s).GraphQL(ctx, query, nil, &out)
		var gql *GraphQLError
		if !errors.As(err, &gql) || !gql.Partial || len(gql.Errors) != 1 || gql.Errors[0].Type != "FORBIDDEN" {
			t.Fatalf("error = %v", err)
		}
		if out.Repository == nil || out.Repository.ID != "R_1" {
			t.Errorf("the partial data was not decoded: %+v", out)
		}
	})
	t.Run("errors with no data are not partial", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", githubtest.JSON(200, map[string]any{"data": nil, "errors": []map[string]any{{"message": "Parse error"}}}))
		var out result
		err := newClient(s).GraphQL(ctx, query, nil, &out)
		var gql *GraphQLError
		if !errors.As(err, &gql) || gql.Partial || !strings.Contains(err.Error(), "Parse error") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("a GraphQL rate limit is the typed rate-limit error", func(t *testing.T) {
		reset := now.Add(30 * time.Minute)
		s := githubtest.New(t)
		s.Handle("POST", "/graphql", githubtest.JSON(200, map[string]any{"errors": []map[string]any{{"type": "RATE_LIMITED", "message": "API rate limit exceeded"}}}).
			WithHeader("X-RateLimit-Remaining", "0").WithHeader("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10)))
		var out result
		err := newClient(s).GraphQL(ctx, query, nil, &out)
		var limited *RateLimitError
		if !errors.As(err, &limited) || limited.Secondary || !limited.Reset.Equal(reset) {
			t.Errorf("error = %v", err)
		}
		if rate, ok := newClient(s).Rate(ResourceGraphQL); ok {
			t.Errorf("a fresh client reports a rate: %+v", rate)
		}
	})
}
