package github

import (
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

func run(id int64, name, status, conclusion, app string) map[string]any {
	return map[string]any{"id": id, "name": name, "status": status, "conclusion": conclusion, "html_url": "https://github.com/acme/widgets/actions/runs/1/job/" + name, "app": map[string]any{"slug": app}}
}

func TestChecks(t *testing.T) {
	t.Run("check runs and commit statuses are read into gh's buckets, the Actions jobs with their ids", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/commits/abc/check-runs", githubtest.JSON(200, map[string]any{"check_runs": []map[string]any{
			run(11, "build", "completed", "success", "github-actions"),
			run(12, "unit", "completed", "failure", "github-actions"),
			run(13, "e2e", "in_progress", "", "github-actions"),
			run(14, "docs", "completed", "skipped", "github-actions"),
			run(15, "deploy", "completed", "cancelled", "github-actions"),
			run(16, "codecov", "completed", "neutral", "codecov"),
			run(17, "slow", "completed", "timed_out", "github-actions"),
		}}))
		s.Handle("GET", "/repos/acme/widgets/commits/abc/status", githubtest.JSON(200, map[string]any{"statuses": []map[string]any{
			{"context": "ci/legacy", "state": "pending", "target_url": "https://ci.example/1"},
			{"context": "ci/old", "state": "error", "target_url": "https://ci.example/2"},
		}}))
		checks, err := newClient(s).Checks(ctx, "acme", "widgets", "abc")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]Bucket{"build": BucketPass, "unit": BucketFail, "e2e": BucketPending, "docs": BucketSkipping, "deploy": BucketCancel, "codecov": BucketSkipping, "slow": BucketFail, "ci/legacy": BucketPending, "ci/old": BucketFail}
		if len(checks) != len(want) {
			t.Fatalf("checks = %+v", checks)
		}
		for _, c := range checks {
			if want[c.Name] != c.Bucket {
				t.Errorf("%s: %s, want %s", c.Name, c.Bucket, want[c.Name])
			}
		}
		if checks[1].Job != 12 || checks[5].Job != 0 || checks[7].Link != "https://ci.example/1" {
			t.Errorf("checks = %+v", checks)
		}
		if q := s.Requests()[0].Query; q.Get("filter") != "latest" {
			t.Errorf("query %v", q)
		}
	})
	t.Run("an unreadable read is an error, never an empty list", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/commits/abc/check-runs", githubtest.Text(502, "bad gateway"))
		if checks, err := newClient(s).Checks(ctx, "acme", "widgets", "abc"); err == nil || checks != nil {
			t.Errorf("checks %v, err %v", checks, err)
		}
	})
	t.Run("a job's log is read as text", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/actions/jobs/12/logs", githubtest.Text(200, "step 1\nFAIL: TestX\n"))
		if log, err := newClient(s).JobLog(ctx, "acme", "widgets", 12); err != nil || log != "step 1\nFAIL: TestX\n" {
			t.Errorf("log %q, err %v", log, err)
		}
	})
}
