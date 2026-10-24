package verify

import (
	"regexp"
	"sort"
	"strings"

	"github.com/benjr70/auto-agent/internal/config"
)

// DefaultViewport is the capture shape of a Surface that declares none: it
// only keeps a tour from having no shape at all. A Target Project whose users
// see a phone or a fixed panel declares the real shape per Surface.
const DefaultViewport = "1280x800"

// tourKinds are the kinds that always earn a Screenshot tour when touched
// (ADR 0003): a project cannot opt out of screenshots by declaration. `cli`
// and `api` Surfaces are evidence only.
var tourKinds = map[string]bool{"browser": true, "electron": true}

// Touched names the Surfaces whose path globs a changed path matches, sorted:
// nil when the diff touched nothing declared, which is an answer, not an
// error. paths are repo-relative; a leading "./" is ignored.
func Touched(paths []string, surfaces map[string]config.Surface) []string {
	return touched(paths, surfaces, false)
}

// Tour is Touched narrowed to the Surfaces that earn a Screenshot tour.
func Tour(paths []string, surfaces map[string]config.Surface) []string {
	return touched(paths, surfaces, true)
}

// Forced is every declared Surface that earns a Screenshot tour, touched or
// not, sorted: a round that must show every UI whatever its diff says.
func Forced(surfaces map[string]config.Surface) []string {
	var tour []string
	for name, s := range surfaces {
		if tourKinds[s.Kind] {
			tour = append(tour, name)
		}
	}
	sort.Strings(tour)
	return tour
}

func touched(paths []string, surfaces map[string]config.Surface, tourOnly bool) []string {
	var hits []string
	for name, s := range surfaces {
		if tourOnly && !tourKinds[s.Kind] {
			continue
		}
		if matchesAny(paths, s.Paths) {
			hits = append(hits, name)
		}
	}
	sort.Strings(hits)
	return hits
}

// Viewport is the capture shape of the named Surface: its declared viewport,
// else DefaultViewport. ok is false when no such Surface is declared, never a
// made-up shape.
func Viewport(surfaces map[string]config.Surface, name string) (viewport string, ok bool) {
	s, ok := surfaces[name]
	switch {
	case !ok:
		return "", false
	case s.Viewport != "":
		return s.Viewport, true
	}
	return DefaultViewport, true
}

func matchesAny(paths, globs []string) bool {
	for _, glob := range globs {
		if glob == "" {
			continue
		}
		re := globRegexp(glob)
		for _, p := range paths {
			if re.MatchString(strings.TrimPrefix(p, "./")) {
				return true
			}
		}
	}
	return false
}

// globRegexp is a path glob as an anchored regular expression: `*` and `?`
// stop at `/`, `**` crosses directories, a pattern ending in `/` matches
// everything beneath it, and every other character is literal.
func globRegexp(glob string) *regexp.Regexp {
	if strings.HasSuffix(glob, "/") {
		glob += "**"
	}
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch {
		case strings.HasPrefix(glob[i:], "**"):
			re.WriteString(".*")
			i++
		case glob[i] == '*':
			re.WriteString("[^/]*")
		case glob[i] == '?':
			re.WriteString("[^/]")
		default:
			re.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String())
}
