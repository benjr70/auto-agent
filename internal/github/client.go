// Package github is the Daemon's GitHub client: typed REST and GraphQL calls
// made directly to the API, with conditional requests so that asking again
// about something unchanged costs nothing against the rate limit.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"
	// secondaryWait is how long to stay away after a secondary limit that
	// names no Retry-After: GitHub asks for at least a minute.
	secondaryWait = time.Minute
)

// Options configures a Client. Only Token is required.
type Options struct {
	// BaseURL is the API root; empty means api.github.com.
	BaseURL string
	// Token authenticates every request (see ResolveToken).
	Token string
	// UserAgent is sent on every request, as GitHub requires one.
	UserAgent string
	// HTTPClient makes the requests; nil means http.DefaultClient.
	HTTPClient *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Client calls the GitHub API as one token. It is safe for concurrent use.
type Client struct {
	baseURL   string
	token     string
	userAgent string
	http      *http.Client
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]cached // by URL: the last 200 that carried an ETag
	rate  *Rate
}

type cached struct {
	etag string
	body []byte
}

// New returns a Client.
func New(o Options) *Client {
	c := &Client{
		baseURL:   strings.TrimSuffix(o.BaseURL, "/"),
		token:     o.Token,
		userAgent: o.UserAgent,
		http:      o.HTTPClient,
		now:       o.Now,
		cache:     map[string]cached{},
	}
	if c.baseURL == "" {
		c.baseURL = defaultBaseURL
	}
	if c.userAgent == "" {
		c.userAgent = "auto-agent"
	}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// Rate is the rate-limit state GitHub reported on a response.
type Rate struct {
	// Resource is the limit the request counted against ("core", "graphql").
	Resource  string
	Limit     int
	Remaining int
	Used      int
	// Reset is when Remaining returns to Limit.
	Reset time.Time
}

// Rate is the rate-limit state from the most recent response that carried
// one; ok is false until there has been such a response.
func (c *Client) Rate() (rate Rate, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rate == nil {
		return Rate{}, false
	}
	return *c.rate, true
}

// APIError is a response GitHub answered with a failure status.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports whether err is GitHub answering 404.
func IsNotFound(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}

// RateLimitError is GitHub refusing a request because a rate limit is spent.
type RateLimitError struct {
	// Secondary is true for an abuse limit, false for the hourly quota.
	Secondary bool
	// Reset is when requests may be made again.
	Reset   time.Time
	Message string
}

func (e *RateLimitError) Error() string {
	kind := "primary"
	if e.Secondary {
		kind = "secondary"
	}
	return fmt.Sprintf("github: %s rate limit reached, resets %s: %s", kind, e.Reset.UTC().Format(time.RFC3339), e.Message)
}

// get fetches path and decodes the JSON response into out. A repeat of a GET
// whose last response carried an ETag is conditional: GitHub answers 304,
// which does not count against the rate limit, and the remembered body is
// decoded instead.
func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// send makes a write request with a JSON body and decodes the response into
// out when out is not nil.
func (c *Client) send(ctx context.Context, method, path string, body, out any) error {
	return c.do(ctx, method, path, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	url := c.baseURL + path
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("github: %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	conditional := method == http.MethodGet
	var remembered cached
	if conditional {
		c.mu.Lock()
		remembered = c.cache[url]
		c.mu.Unlock()
		if remembered.etag != "" {
			req.Header.Set("If-None-Match", remembered.etag)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("github: %s %s: reading the response: %w", method, path, err)
	}
	rate := c.noteRate(resp.Header)

	switch {
	case resp.StatusCode == http.StatusNotModified && remembered.etag != "":
		data = remembered.body
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if etag := resp.Header.Get("ETag"); conditional && etag != "" {
			c.mu.Lock()
			c.cache[url] = cached{etag: etag, body: data}
			c.mu.Unlock()
		}
	default:
		return c.failure(method, path, resp, data, rate)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: %s %s: decoding the response: %w", method, path, err)
	}
	return nil
}

// noteRate remembers the rate-limit headers of a response, when it has them.
func (c *Client) noteRate(h http.Header) *Rate {
	if h.Get("X-RateLimit-Remaining") == "" {
		return nil
	}
	rate := &Rate{
		Resource:  h.Get("X-RateLimit-Resource"),
		Limit:     atoi(h.Get("X-RateLimit-Limit")),
		Remaining: atoi(h.Get("X-RateLimit-Remaining")),
		Used:      atoi(h.Get("X-RateLimit-Used")),
	}
	if reset := atoi(h.Get("X-RateLimit-Reset")); reset > 0 {
		rate.Reset = time.Unix(int64(reset), 0)
	}
	c.mu.Lock()
	c.rate = rate
	c.mu.Unlock()
	return rate
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// failure turns a failure response into a RateLimitError when a limit is
// what refused it, and an APIError otherwise.
func (c *Client) failure(method, path string, resp *http.Response, data []byte, rate *Rate) error {
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) != nil || body.Message == "" {
		body.Message = strings.TrimSpace(string(data))
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := resp.Header.Get("Retry-After")
		switch {
		case retryAfter != "":
			return &RateLimitError{Secondary: true, Reset: c.now().Add(time.Duration(atoi(retryAfter)) * time.Second), Message: body.Message}
		case rate != nil && rate.Remaining == 0:
			return &RateLimitError{Reset: rate.Reset, Message: body.Message}
		case strings.Contains(strings.ToLower(body.Message), "secondary rate limit"):
			return &RateLimitError{Secondary: true, Reset: c.now().Add(secondaryWait), Message: body.Message}
		}
	}
	return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: body.Message}
}

// GraphQLError is the errors a GraphQL response carried. With Partial set
// the response had data too, which was decoded: a caller that can work with
// what came back may carry on.
type GraphQLError struct {
	Errors  []GraphQLErrorItem
	Partial bool
}

// GraphQLErrorItem is one entry of a GraphQL response's errors.
type GraphQLErrorItem struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

func (e *GraphQLError) Error() string {
	messages := make([]string, len(e.Errors))
	for i, item := range e.Errors {
		messages[i] = item.Message
	}
	return "github: graphql: " + strings.Join(messages, "; ")
}

// GraphQL runs a query or mutation and decodes its data into out. When the
// response carries errors it returns a *GraphQLError, after decoding whatever
// data came with them.
func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	request := struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables,omitempty"`
	}{query, variables}
	var response struct {
		Data   json.RawMessage    `json:"data"`
		Errors []GraphQLErrorItem `json:"errors"`
	}
	if err := c.send(ctx, http.MethodPost, "/graphql", request, &response); err != nil {
		return err
	}
	hasData := len(response.Data) > 0 && string(response.Data) != "null"
	if hasData && out != nil {
		if err := json.Unmarshal(response.Data, out); err != nil {
			return fmt.Errorf("github: graphql: decoding the data: %w", err)
		}
	}
	if len(response.Errors) == 0 {
		return nil
	}
	for _, item := range response.Errors {
		if item.Type == "RATE_LIMITED" {
			limited := &RateLimitError{Message: item.Message, Reset: c.now().Add(secondaryWait)}
			if rate, ok := c.Rate(); ok && !rate.Reset.IsZero() {
				limited.Reset = rate.Reset
			}
			return limited
		}
	}
	return &GraphQLError{Errors: response.Errors, Partial: hasData}
}
