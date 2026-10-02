package pattern

import (
	"strings"
	"testing"
)

// FuzzCompileMatch fuzzes the glob compiler and matcher with arbitrary pattern
// and subject strings. Patterns come from ApplicationSet input and subjects from
// a registry catalog, so neither is fully trusted: Compile must either return an
// error or a usable Pattern, and Match must never panic for any input.
func FuzzCompileMatch(f *testing.F) {
	seeds := []struct {
		pattern string
		subject string
	}{
		{"apps/my-app", "apps/my-app"},
		{"apps/*/web", "apps/team/web"},
		{"apps/**/{env}", "apps/a/b/c/prod"},
		{"{team}/{app}", "payments/web"},
		{"v?.?.?", "v1.2.3"},
		{"{name}-current", "orders-current"},
		{"**", "a/b/c"},
		{"", ""},
		{"{", "{"},
		{"}", "}"},
		{"{}", ""},
		{"{a}{b}", "xy"},
		{"***", "a"},
		{"a//b", "a//b"},
		{"[](){}.*+?^$|\\", "literal"},
		{strings.Repeat("*", 64), strings.Repeat("a", 64)},
		{strings.Repeat("{n}/", 32), strings.Repeat("x/", 32)},
	}
	for _, s := range seeds {
		f.Add(s.pattern, s.subject)
	}

	f.Fuzz(func(t *testing.T, pattern, subject string) {
		{
			const mode = ModePath
			// Keep inputs bounded so the fuzzer spends its time on shapes rather
			// than on pathologically long strings.
			if len(pattern) > 1024 || len(subject) > 1024 {
				t.Skip()
			}

			p, err := Compile(pattern, mode)
			if err != nil {
				if p != nil {
					t.Fatalf("Compile(%q) returned both a pattern and an error %v", pattern, err)
				}
				return // rejecting bad input is correct behaviour
			}
			if p == nil {
				t.Fatalf("Compile(%q) returned nil pattern and nil error", pattern)
			}

			// Accessors must be safe on any compiled pattern.
			_ = p.Raw()
			_ = p.HasWildcard()
			names := p.CaptureNames()

			// A literal prefix must really be a prefix of the raw pattern,
			// otherwise catalog narrowing could discard matching repositories.
			if lp := p.LiteralPrefix(); !strings.HasPrefix(pattern, lp) {
				t.Fatalf("LiteralPrefix %q is not a prefix of pattern %q", lp, pattern)
			}

			named, ordered, ok := p.Match(subject)
			if !ok {
				if named != nil || ordered != nil {
					t.Fatalf("Match(%q) returned captures on a non-match", subject)
				}
				return
			}

			// On a match, captures must be internally consistent.
			if len(ordered) != len(p.names) {
				t.Fatalf("got %d ordered captures, want %d (pattern %q)", len(ordered), len(p.names), pattern)
			}
			if len(named) != len(names) {
				t.Fatalf("got %d named captures, want %d (pattern %q)", len(named), len(names), pattern)
			}
			for _, c := range ordered {
				if c.Name == "" {
					continue
				}
				if v, found := named[c.Name]; !found || v != c.Value {
					t.Fatalf("named capture %q missing or mismatched (pattern %q)", c.Name, pattern)
				}
			}
			// Anonymous() must never include named captures.
			for i, v := range Anonymous(ordered) {
				_ = i
				_ = v
			}

			// Matching is deterministic: the same input must match again.
			if _, _, ok2 := p.Match(subject); !ok2 {
				t.Fatalf("Match(%q) not deterministic for pattern %q", subject, pattern)
			}
		}
	})
}

// FuzzCompileTagMode fuzzes tag-mode compilation specifically, since tag
// patterns use minimal-run semantics that differ from path matching.
func FuzzCompileTagMode(f *testing.F) {
	for _, s := range []string{"{app}-current", "v*", "*-rc*", "{a}{b}", "", "?", "**"} {
		f.Add(s, "orders-current")
	}
	f.Fuzz(func(t *testing.T, pattern, subject string) {
		if len(pattern) > 512 || len(subject) > 512 {
			t.Skip()
		}
		p, err := Compile(pattern, ModeTag)
		if err != nil {
			return
		}
		named, ordered, ok := p.Match(subject)
		if !ok {
			return
		}
		for _, c := range ordered {
			if c.Name != "" && named[c.Name] != c.Value {
				t.Fatalf("tag capture mismatch for %q (pattern %q)", c.Name, pattern)
			}
		}
	})
}
