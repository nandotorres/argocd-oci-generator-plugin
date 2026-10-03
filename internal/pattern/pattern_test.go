package pattern

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileErrors(t *testing.T) {
	for _, p := range []string{"", "a/{unclosed", "a/{bad-name}", "a/{dup}/{dup}"} {
		_, err := Compile(p)
		assert.Error(t, err, "pattern %q should fail", p)
	}
}

func TestLiteralNoWildcard(t *testing.T) {
	p, err := Compile("apps-oci/orders-api/dev")
	require.NoError(t, err)
	assert.False(t, p.HasWildcard())
	assert.Equal(t, "apps-oci/orders-api/dev", p.LiteralPrefix())

	_, _, ok := p.Match("apps-oci/orders-api/dev")
	assert.True(t, ok)
	_, _, ok = p.Match("apps-oci/orders-api/prod")
	assert.False(t, ok)
}

func TestDotIsLiteral(t *testing.T) {
	p, err := Compile("repo/v1.2.3")
	require.NoError(t, err)
	_, _, ok := p.Match("repo/v1.2.3")
	assert.True(t, ok)
	_, _, ok = p.Match("repo/v1X2X3")
	assert.False(t, ok, "'.' must be literal, not regex any-char")
}

func TestSingleSegmentStar(t *testing.T) {
	p, err := Compile("a/*/c")
	require.NoError(t, err)
	assert.True(t, p.HasWildcard())
	assert.Equal(t, "a/", p.LiteralPrefix())

	_, ordered, ok := p.Match("a/b/c")
	require.True(t, ok)
	assert.Equal(t, []string{"b"}, Anonymous(ordered))

	_, _, ok = p.Match("a/b/x/c")
	assert.False(t, ok, "'*' must not cross a separator")
}

func TestNamedCaptures(t *testing.T) {
	p, err := Compile("apps-oci/{team}/{env}")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"team", "env"}, p.CaptureNames())

	named, _, ok := p.Match("apps-oci/payments/dev")
	require.True(t, ok)
	assert.Equal(t, map[string]string{"team": "payments", "env": "dev"}, named)

	_, _, ok = p.Match("apps-oci/payments/extra/dev")
	assert.False(t, ok, "named capture is a single segment")
}

func TestGlobstarMiddle(t *testing.T) {
	p, err := Compile("a/**/b")
	require.NoError(t, err)

	cases := map[string]struct {
		ok       bool
		wildcard string
	}{
		"a/b":     {true, ""},
		"a/x/b":   {true, "x"},
		"a/x/y/b": {true, "x/y"},
		"a/b/c":   {false, ""},
	}
	for in, want := range cases {
		named, ordered, ok := p.Match(in)
		assert.Equal(t, want.ok, ok, "input %q", in)
		if want.ok {
			assert.Empty(t, named)
			assert.Equal(t, want.wildcard, Anonymous(ordered)[0], "input %q wildcard", in)
		}
	}
}

func TestGlobstarLeadingAndTrailing(t *testing.T) {
	lead, err := Compile("**/dev")
	require.NoError(t, err)
	for in, want := range map[string]bool{"dev": true, "a/dev": true, "a/b/dev": true, "dev/x": false} {
		_, _, ok := lead.Match(in)
		assert.Equal(t, want, ok, "leading input %q", in)
	}

	trail, err := Compile("apps-oci/**")
	require.NoError(t, err)
	assert.Equal(t, "apps-oci/", trail.LiteralPrefix())
	for in, want := range map[string]bool{
		"apps-oci":       true,
		"apps-oci/a":     true,
		"apps-oci/a/b/c": true,
		"other/a":        false,
	} {
		_, _, ok := trail.Match(in)
		assert.Equal(t, want, ok, "trailing input %q", in)
	}
}

// The user's canonical example, in our syntax.
func TestUserExampleRepoAndTag(t *testing.T) {
	repo, err := Compile("apps-oci/**/{env}")
	require.NoError(t, err)
	named, ordered, ok := repo.Match("apps-oci/orders-api/orders-api/dev")
	require.True(t, ok)
	assert.Equal(t, "dev", named["env"])
	assert.Equal(t, []string{"orders-api/orders-api"}, Anonymous(ordered))

	tag, err := Compile("{something}-current")
	require.NoError(t, err)
	tnamed, _, ok := tag.Match("dev-current")
	require.True(t, ok)
	assert.Equal(t, "dev", tnamed["something"])

	tnamed, _, ok = tag.Match("orders-api-current")
	require.True(t, ok)
	assert.Equal(t, "orders-api", tnamed["something"], "minimal-run capture handles hyphens")

	_, _, ok = tag.Match("dev-stable")
	assert.False(t, ok)
}

func TestTagModeStarMatchesAcrossHyphens(t *testing.T) {
	p, err := Compile("v1.*")
	require.NoError(t, err)
	for in, want := range map[string]bool{"v1.2.3": true, "v1.": true, "v2.0.0": false} {
		_, _, ok := p.Match(in)
		assert.Equal(t, want, ok, "tag input %q", in)
	}
}

func TestQuestionMark(t *testing.T) {
	p, err := Compile("v?")
	require.NoError(t, err)
	_, _, ok := p.Match("v1")
	assert.True(t, ok)
	_, _, ok = p.Match("v12")
	assert.False(t, ok)
}

// Found by FuzzCompileMatch: a pattern containing invalid UTF-8 used to compile
// successfully, but the literal prefix was built from decoded runes and so
// contained U+FFFD, making it not a prefix of the pattern at all. Reject such
// patterns instead of silently mangling them.
func TestCompileRejectsInvalidUTF8(t *testing.T) {
	p, err := Compile("apps/\xc2bad/**")
	assert.Error(t, err, "invalid UTF-8 pattern must be rejected")
	assert.Nil(t, p)
}
