package github

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"testing"

	"github.com/benjr70/auto-agent/internal/github/githubtest"
)

func TestIdentity(t *testing.T) {
	tests := []struct {
		name  string
		reply githubtest.Reply
		want  User
	}{
		{
			name:  "a classic token names its scopes",
			reply: githubtest.JSON(200, map[string]string{"login": "machine-user"}).WithHeader("X-OAuth-Scopes", "repo, read:project, workflow"),
			want:  User{Login: "machine-user", Classic: true, Scopes: []string{"repo", "read:project", "workflow"}},
		},
		{
			name:  "a classic token with no scopes is still classic",
			reply: githubtest.JSON(200, map[string]string{"login": "machine-user"}).WithHeader("X-OAuth-Scopes", ""),
			want:  User{Login: "machine-user", Classic: true},
		},
		{
			name:  "a fine-grained token names none",
			reply: githubtest.JSON(200, map[string]string{"login": "machine-user"}),
			want:  User{Login: "machine-user"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := githubtest.New(t)
			s.Handle("GET", "/user", tt.reply)
			got, err := newClient(s).User(ctx)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("User = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
	t.Run("a repository says what the token may do there, and nil when it does not say", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets", githubtest.JSON(200, map[string]any{"full_name": "acme/widgets", "permissions": map[string]bool{"pull": true, "push": true}}))
		s.Handle("GET", "/repos/acme/gadgets", githubtest.JSON(200, map[string]any{"full_name": "acme/gadgets"}))
		widgets, err := newClient(s).Repository(ctx, "acme", "widgets")
		if err != nil || widgets.Permissions == nil || !widgets.Permissions.Push || !widgets.Permissions.Pull || widgets.Permissions.Admin {
			t.Errorf("widgets = %+v, %v", widgets, err)
		}
		if gadgets, err := newClient(s).Repository(ctx, "acme", "gadgets"); err != nil || gadgets.Permissions != nil {
			t.Errorf("gadgets = %+v, %v", gadgets, err)
		}
	})
	t.Run("the labels are read page by page until a short one", func(t *testing.T) {
		s := githubtest.New(t)
		full := make([]map[string]string, 100)
		for i := range full {
			full[i] = map[string]string{"name": fmt.Sprintf("l%d", i)}
		}
		s.HandleFunc("GET", "/repos/acme/widgets/labels", func(r githubtest.Request) githubtest.Reply {
			if r.Query.Get("page") == "1" {
				return githubtest.JSON(200, full)
			}
			return githubtest.JSON(200, []map[string]string{{"name": "AFK"}})
		})
		labels, err := newClient(s).Labels(ctx, "acme", "widgets")
		if err != nil || len(labels) != 101 || labels[0] != "l0" || labels[100] != "AFK" {
			t.Errorf("Labels = %d labels, %v", len(labels), err)
		}
	})
	t.Run("a file is read from the default branch, and a missing one is not found", func(t *testing.T) {
		s := githubtest.New(t)
		s.Handle("GET", "/repos/acme/widgets/contents/.auto-agent/harness.json", githubtest.JSON(200, map[string]string{
			"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(`{"pick":{}}`))[:6] + "\n" + base64.StdEncoding.EncodeToString([]byte(`{"pick":{}}`))[6:],
		}))
		s.Handle("GET", "/repos/acme/gadgets/contents/.auto-agent/harness.json", githubtest.Text(404, `{"message":"Not Found"}`))
		if got, err := newClient(s).FileContent(ctx, "acme", "widgets", ".auto-agent/harness.json"); err != nil || string(got) != `{"pick":{}}` {
			t.Errorf("widgets = %q, %v", got, err)
		}
		if _, err := newClient(s).FileContent(ctx, "acme", "gadgets", ".auto-agent/harness.json"); !IsNotFound(err) {
			t.Errorf("gadgets err = %v, want not found", err)
		}
	})
}
