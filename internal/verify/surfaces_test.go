package verify

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/benjr70/auto-agent/internal/config"
)

var declared = map[string]config.Surface{
	"web":   {Kind: "browser", URLKey: "WEB_URL", Paths: []string{"apps/web/src/**", "apps/web/public/"}, Viewport: "427x952"},
	"panel": {Kind: "electron", URLKey: "PANEL_URL", Paths: []string{"apps/panel/**"}, Launcher: "verify/panel"},
	"cli":   {Kind: "cli", URLKey: "CLI_BIN", Paths: []string{"cmd/*.go"}},
	"api":   {Kind: "api", URLKey: "API_URL", Paths: []string{"services/api/**"}},
}

// Ported from lib/surfaces.test.sh, under its case names. Dropped: "list"
// (the declaration is already a map Go reads); "usage: unknown subcommand",
// "usage: viewport needs a name" and "--pr: paths from gh pr diff" (no CLI:
// the caller hands Touched the paths github.Client.Files read); "--head: the
// checkout wins" and "without --head: the inherited config still wins" (Go
// reads no inherited config: the caller passes the Surfaces of the config it
// loaded, and BootRound loads the head's, see TestBootRound); and
// "viewport: the default is overridable" (SURFACES_DEFAULT_VIEWPORT was a
// Host env seam no Go caller sets; a Target Project declares its real shape
// per Surface, so DefaultViewport is a constant).
func TestTouched(t *testing.T) {
	for _, tt := range []struct {
		name  string
		paths []string
		want  []string
	}{
		{"touched: two Surfaces", []string{"apps/web/src/app.tsx", "services/api/main.go"}, []string{"api", "web"}},
		{"touched: empty is an answer", []string{"README.md", "docs/adr/0001.md"}, nil},
		{"glob: cmd/*.go does not match cmd/sub/deep.go", []string{"cmd/sub/deep.go"}, nil},
		{"glob: cmd/*.go matches cmd/main.go", []string{"cmd/main.go"}, []string{"cli"}},
		{"glob: ** crosses directories", []string{"apps/panel/src/deep/main.ts"}, []string{"panel"}},
		{"glob: trailing slash", []string{"apps/web/public/logo.svg"}, []string{"web"}},
		{"glob: leading ./", []string{"./apps/web/src/app.tsx"}, []string{"web"}},
		{"glob: ? is one character, never a slash", []string{"cmd/a/b.go"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Touched(tt.paths, declared); !slices.Equal(got, tt.want) {
				t.Errorf("Touched = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("glob: . is literal", func(t *testing.T) {
		dotted := map[string]config.Surface{"one": {Kind: "browser", URLKey: "U", Paths: []string{"app/v1.2/**"}}}
		if got := Touched([]string{"app/v1X2/index.tsx"}, dotted); got != nil {
			t.Errorf("Touched = %q", got)
		}
		if got := Touched([]string{"app/v1.2/index.tsx"}, dotted); !slices.Equal(got, []string{"one"}) {
			t.Errorf("Touched = %q", got)
		}
	})
}

func TestTour(t *testing.T) {
	// Behaviour 1: a touched browser Surface earns a tour; a touched cli
	// Surface does not.
	t.Run("tour: UI kinds only", func(t *testing.T) {
		got := Tour([]string{"apps/web/src/app.tsx", "apps/panel/main.ts", "cmd/main.go", "services/api/main.go"}, declared)
		if !slices.Equal(got, []string{"panel", "web"}) {
			t.Errorf("Tour = %q", got)
		}
	})
	t.Run("tour: evidence-only kinds", func(t *testing.T) {
		if got := Tour([]string{"cmd/main.go", "services/api/main.go"}, declared); got != nil {
			t.Errorf("Tour = %q", got)
		}
	})
}

func TestForced(t *testing.T) {
	// A forced tour is every UI Surface, whatever the diff touched.
	if got := Forced(declared); !slices.Equal(got, []string{"panel", "web"}) {
		t.Errorf("Forced = %q", got)
	}
	if got := Forced(nil); got != nil {
		t.Errorf("Forced(nil) = %q", got)
	}
}

func TestViewport(t *testing.T) {
	for _, tt := range []struct {
		name, surface, want string
		ok                  bool
	}{
		{"viewport: declared", "web", "427x952", true},
		{"viewport: default", "panel", DefaultViewport, true},
		{"viewport: unknown Surface", "nope", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := Viewport(declared, tt.surface); got != tt.want || ok != tt.ok {
				t.Errorf("Viewport(%s) = %q, %v", tt.surface, got, ok)
			}
		})
	}
	if DefaultViewport != "1280x800" {
		t.Errorf("DefaultViewport = %s", DefaultViewport)
	}
}

// "fixture: the browser Surface earns the tour", "fixture: the declared
// viewport": the fixture Target Project's own declaration answers.
func TestSurfacesFixture(t *testing.T) {
	h, err := config.LoadHarness(filepath.Join("..", "..", "plugin", "fixtures", "target-project"), "", fixtureRepo)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("fixture: the browser Surface earns the tour", func(t *testing.T) {
		if got := Tour([]string{"app/server.py"}, h.Surfaces); !slices.Equal(got, []string{"web"}) {
			t.Errorf("Tour = %q", got)
		}
	})
	t.Run("fixture: the declared viewport", func(t *testing.T) {
		if got, _ := Viewport(h.Surfaces, "web"); got != "1024x768" {
			t.Errorf("Viewport = %q", got)
		}
	})
}

var fixtureRepo = config.Repo{Owner: "acme", Name: "widgets", DefaultBranch: "main"}
