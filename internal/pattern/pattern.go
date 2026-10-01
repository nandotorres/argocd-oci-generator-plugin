// Package pattern implements the glob-with-captures matching used to select OCI
// repositories (across the registry catalog) and tags.
//
// It intentionally mirrors the ergonomics of the ArgoCD Git directory generator
// (which uses path.Match globs) and extends them with:
//
//   - the "**" globstar (zero or more path segments), and
//   - named captures "{name}" plus positional captures for "*"/"**"/"?".
//
// The syntax is glob, not regexp: "." is a literal. Use tagFilters (regex) when
// you need full regular expressions.
package pattern

import (
	"fmt"
	"regexp"
	"strings"
)

// Mode selects segment-aware (path) matching versus flat (tag) matching.
type Mode int

const (
	// ModePath treats "/" as a segment separator: "*" matches a single segment
	// and "**" matches zero or more segments (globstar).
	ModePath Mode = iota
	// ModeTag treats the whole input as a single token with no separators: "*"
	// matches any run of characters and "**" is not special.
	ModeTag
)

// Capture is a single matched wildcard value. Name is empty for anonymous
// wildcards ("*", "**", "?"); those are addressable positionally instead.
type Capture struct {
	Name  string
	Value string
}

// Pattern is a compiled glob pattern.
type Pattern struct {
	raw           string
	re            *regexp.Regexp
	names         []string // per capture group, in order; "" when anonymous
	hasWildcard   bool
	literalPrefix string
}

// Raw returns the original pattern string.
func (p *Pattern) Raw() string { return p.raw }

// HasWildcard reports whether the pattern contains any wildcard or capture.
func (p *Pattern) HasWildcard() bool { return p.hasWildcard }

// LiteralPrefix returns the literal prefix before the first wildcard. It is used
// to cheaply narrow a registry catalog before full matching.
func (p *Pattern) LiteralPrefix() string { return p.literalPrefix }

// CaptureNames returns the declared named captures, in order of appearance.
func (p *Pattern) CaptureNames() []string {
	var out []string
	for _, n := range p.names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// Match reports whether s matches the pattern, returning the ordered captures
// (named and anonymous) when it does.
func (p *Pattern) Match(s string) (named map[string]string, ordered []Capture, ok bool) {
	m := p.re.FindStringSubmatch(s)
	if m == nil {
		return nil, nil, false
	}
	named = map[string]string{}
	ordered = make([]Capture, 0, len(p.names))
	for i, name := range p.names {
		val := m[i+1]
		ordered = append(ordered, Capture{Name: name, Value: val})
		if name != "" {
			named[name] = val
		}
	}
	return named, ordered, true
}

// Anonymous returns only the positional (anonymous) captures from ordered, in
// order. Named captures are excluded.
func Anonymous(ordered []Capture) []string {
	var out []string
	for _, c := range ordered {
		if c.Name == "" {
			out = append(out, c.Value)
		}
	}
	return out
}

// Compile compiles a glob pattern in the given mode.
func Compile(p string, mode Mode) (*Pattern, error) {
	if p == "" {
		return nil, fmt.Errorf("pattern must not be empty")
	}

	var (
		b            strings.Builder
		names        []string
		hasWildcard  bool
		litPrefix    strings.Builder
		prefixClosed bool
	)
	b.WriteString("^")

	addLiteral := func(r rune) {
		b.WriteString(regexp.QuoteMeta(string(r)))
		if !prefixClosed {
			litPrefix.WriteRune(r)
		}
	}
	openWildcard := func(name, expr string) {
		names = append(names, name)
		hasWildcard = true
		prefixClosed = true
		b.WriteString(expr)
	}

	runes := []rune(p)
	seenName := map[string]bool{}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '{':
			end := indexRune(runes, i+1, '}')
			if end < 0 {
				return nil, fmt.Errorf("unterminated '{' in pattern %q", p)
			}
			name := string(runes[i+1 : end])
			if err := validateName(name); err != nil {
				return nil, fmt.Errorf("invalid capture %q in pattern %q: %w", name, p, err)
			}
			if seenName[name] {
				return nil, fmt.Errorf("duplicate capture name %q in pattern %q", name, p)
			}
			seenName[name] = true
			// Named capture: one segment (path) or a minimal run (tag).
			openWildcard(name, "([^/]+?)")
			i = end
		case r == '*':
			if mode == ModePath && i+1 < len(runes) && runes[i+1] == '*' {
				// Globstar: zero or more path segments. Absorb a neighbouring
				// separator so "a/**/b" also matches "a/b".
				i++ // consume second '*'
				prevSlash := endsWith(&b, "/")
				nextSlash := i+1 < len(runes) && runes[i+1] == '/'
				switch {
				case nextSlash:
					// "**/" or "/**/": keep the leading slash (if any) and make
					// the run of "<segment>/" optional so "a/**/b" matches "a/b".
					openWildcard("", "(?:(.*)/)?")
					i++ // consume following '/'
				case prevSlash:
					// trailing "/**": absorb the leading slash so "a/**" matches "a".
					trimTrailingSlash(&b)
					openWildcard("", "(?:/(.*))?")
				default:
					openWildcard("", "(.*)")
				}
			} else if mode == ModePath {
				openWildcard("", "([^/]*)")
			} else {
				openWildcard("", "(.*)")
			}
		case r == '?':
			if mode == ModePath {
				openWildcard("", "([^/])")
			} else {
				openWildcard("", "(.)")
			}
		default:
			addLiteral(r)
		}
	}
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("failed to compile pattern %q: %w", p, err)
	}
	return &Pattern{
		raw:           p,
		re:            re,
		names:         names,
		hasWildcard:   hasWildcard,
		literalPrefix: litPrefix.String(),
	}, nil
}

func indexRune(runes []rune, from int, target rune) int {
	for i := from; i < len(runes); i++ {
		if runes[i] == target {
			return i
		}
	}
	return -1
}

func endsWith(b *strings.Builder, suffix string) bool {
	return strings.HasSuffix(b.String(), suffix)
}

func trimTrailingSlash(b *strings.Builder) {
	s := b.String()
	s = strings.TrimSuffix(s, "/")
	b.Reset()
	b.WriteString(s)
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	for _, r := range name {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("name may only contain [A-Za-z0-9_]")
		}
	}
	return nil
}
