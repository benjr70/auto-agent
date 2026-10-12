package githubtest

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func get(t *testing.T, url string, header ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestServer(t *testing.T) {
	t.Run("the fake records requests in order", func(t *testing.T) {
		s := New(t)
		s.Handle("GET", "/user", JSON(200, map[string]string{"login": "machine-user"}))
		s.Handle("POST", "/repos/o/n/issues/1/labels", JSON(200, []string{}))
		get(t, s.URL+"/user?per_page=1", "Authorization", "Bearer t")
		resp, err := http.Post(s.URL+"/repos/o/n/issues/1/labels", "application/json", strings.NewReader(`{"labels":["AFK"]}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got := s.Requests()
		if len(got) != 2 {
			t.Fatalf("recorded %d requests, want 2", len(got))
		}
		if got[0].Method != "GET" || got[0].Path != "/user" || got[0].Query.Get("per_page") != "1" || got[0].Header.Get("Authorization") != "Bearer t" {
			t.Errorf("first request = %+v", got[0])
		}
		if got[1].Method != "POST" || got[1].Path != "/repos/o/n/issues/1/labels" || got[1].Body != `{"labels":["AFK"]}` {
			t.Errorf("second request = %+v", got[1])
		}
	})
	t.Run("scripted replies are served in turn and the last one repeats", func(t *testing.T) {
		s := New(t)
		s.Handle("GET", "/x", Text(500, "boom"), JSON(200, map[string]int{"n": 1}))
		first, _ := get(t, s.URL+"/x")
		second, body := get(t, s.URL+"/x")
		third, _ := get(t, s.URL+"/x")
		if first.StatusCode != 500 || second.StatusCode != 200 || third.StatusCode != 200 || body != `{"n":1}` {
			t.Errorf("statuses = %d %d %d, body = %q", first.StatusCode, second.StatusCode, third.StatusCode, body)
		}
	})
	t.Run("a reply with an ETag answers 304 to a matching If-None-Match", func(t *testing.T) {
		s := New(t)
		s.Handle("GET", "/x", JSON(200, map[string]int{"n": 1}).WithETag(`"v1"`))
		first, _ := get(t, s.URL+"/x")
		second, body := get(t, s.URL+"/x", "If-None-Match", first.Header.Get("ETag"))
		stale, _ := get(t, s.URL+"/x", "If-None-Match", `"v0"`)
		if first.Header.Get("ETag") != `"v1"` || second.StatusCode != 304 || body != "" || stale.StatusCode != 200 {
			t.Errorf("etag = %q, second = %d %q, stale = %d", first.Header.Get("ETag"), second.StatusCode, body, stale.StatusCode)
		}
	})
	t.Run("a 304 can be scripted outright, with headers", func(t *testing.T) {
		s := New(t)
		s.Handle("GET", "/x", Reply{Status: 304}.WithHeader("X-RateLimit-Remaining", "4999"))
		resp, _ := get(t, s.URL+"/x")
		if resp.StatusCode != 304 || resp.Header.Get("X-RateLimit-Remaining") != "4999" {
			t.Errorf("status = %d, header = %q", resp.StatusCode, resp.Header.Get("X-RateLimit-Remaining"))
		}
	})
	t.Run("writes are the requests that change something", func(t *testing.T) {
		s := New(t)
		s.Handle("GET", "/user", JSON(200, nil))
		s.Handle("POST", "/graphql", JSON(200, nil))
		s.Handle("DELETE", "/repos/o/n/issues/1/labels/AFK", JSON(200, nil))
		get(t, s.URL+"/user")
		for _, body := range []string{
			`{"query":"query { viewer { login } }"}`, `{"query":"{ viewer { login } }"}`, `{"query":"# no mutation here\n{ viewer { login } }"}`,
			`{"query":"  mutation { x }"}`, `{"query":"# a comment first\nfragment F on Issue { id }\nmutation M { x }"}`, `not json`,
		} {
			resp, err := http.Post(s.URL+"/graphql", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
		req, _ := http.NewRequest("DELETE", s.URL+"/repos/o/n/issues/1/labels/AFK", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		writes := s.Writes()
		if len(writes) != 4 || writes[0].Path != "/graphql" || writes[3].Method != "DELETE" {
			t.Errorf("writes = %+v, want the two mutations, the unreadable post and the DELETE", writes)
		}
	})
	t.Run("GraphQL replies are scripted by what the document contains", func(t *testing.T) {
		s := New(t)
		s.HandleGraphQL("viewer", Text(200, "viewer"))
		s.HandleGraphQL("repository", Text(200, "repo-1"), Text(200, "repo-2"))
		s.Handle("POST", "/graphql", Text(200, "other"))
		var got []string
		for _, q := range []string{"{ repository { id } }", "{ viewer { login } }", "{ repository { id } }", "{ rateLimit { cost } }"} {
			resp, err := http.Post(s.URL+"/graphql", "application/json", strings.NewReader(`{"query":"`+q+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			got = append(got, string(body))
		}
		if strings.Join(got, ",") != "repo-1,viewer,repo-2,other" {
			t.Errorf("replies = %q", got)
		}
	})
	t.Run("a request nothing was scripted for fails the test and answers 404", func(t *testing.T) {
		rec := &recorder{}
		s := New(rec)
		defer s.Close()
		resp, _ := get(t, s.URL+"/nope")
		if resp.StatusCode != 404 || len(rec.errors) != 1 || !strings.Contains(rec.errors[0], "GET /nope") {
			t.Errorf("status = %d, errors = %q", resp.StatusCode, rec.errors)
		}
	})
}

type recorder struct{ errors []string }

func (r *recorder) Helper()        {}
func (r *recorder) Cleanup(func()) {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
