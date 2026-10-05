package generator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/oci"
)

type fakeClient struct {
	tags  map[string][]string     // "registry/repo" -> tags ("" key absent = repo absent)
	arts  map[string]oci.Artifact // "registry/repo:tag" -> artifact
	err   map[string]error        // method -> error
	calls map[string]int
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		tags:  map[string][]string{},
		arts:  map[string]oci.Artifact{},
		err:   map[string]error{},
		calls: map[string]int{},
	}
}

// TagExists mirrors the real client: an absent repository is an error, an
// absent tag in an existing repository is simply false.
func (f *fakeClient) TagExists(_ context.Context, registry, repo, tag string) (bool, error) {
	f.calls["TagExists"]++
	if err := f.err["TagExists"]; err != nil {
		return false, err
	}
	tags, ok := f.tags[registry+"/"+repo]
	if !ok {
		return false, errors.New("repository does not exist")
	}
	for _, t := range tags {
		if t == tag {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeClient) Head(_ context.Context, registry, repo, tag string) (*oci.Artifact, error) {
	f.calls["Head"]++
	if err := f.err["Head"]; err != nil {
		return nil, err
	}
	a, ok := f.arts[registry+"/"+repo+":"+tag]
	if !ok {
		return nil, errors.New("not found")
	}
	return &a, nil
}

// RepositoryExists mirrors the real client: an absent repository is a
// definitive (false, nil), never an error.
func (f *fakeClient) RepositoryExists(_ context.Context, registry, repo string) (bool, error) {
	f.calls["RepositoryExists"]++
	if err := f.err["RepositoryExists"]; err != nil {
		return false, err
	}
	_, ok := f.tags[registry+"/"+repo]
	return ok, nil
}

// RepositoryHasTags reports true only when the repository exists and is not
// empty.
func (f *fakeClient) RepositoryHasTags(_ context.Context, registry, repo string) (bool, error) {
	f.calls["RepositoryHasTags"]++
	if err := f.err["RepositoryHasTags"]; err != nil {
		return false, err
	}
	tags, ok := f.tags[registry+"/"+repo]
	return ok && len(tags) > 0, nil
}

func (f *fakeClient) addArtifact(a oci.Artifact) {
	f.tags[a.Registry+"/"+a.Repository] = append(f.tags[a.Registry+"/"+a.Repository], a.Tag)
	f.arts[a.Registry+"/"+a.Repository+":"+a.Tag] = a
}

func mustCompile(t *testing.T, in Input) *Query {
	t.Helper()
	q, err := in.Compile("")
	require.NoError(t, err)
	return q
}

func TestTagPresentYieldsOneParamSet(t *testing.T) {
	f := newFakeClient()
	f.addArtifact(oci.Artifact{
		Registry: "r", Repository: "apps/app", Tag: "prod-current",
		Digest: "sha256:abc", MediaType: "application/vnd.oci.image.manifest.v1+json",
	})

	got, err := New(f, nil).Generate(context.Background(),
		mustCompile(t, Input{Registry: "r", Repository: "apps/app", Tag: "prod-current"}))
	require.NoError(t, err)
	require.Len(t, got, 1)

	o := got[0]["oci"].(map[string]any)
	assert.Equal(t, "prod-current", o["tag"])
	assert.Equal(t, "sha256:abc", o["digest"])
	assert.Equal(t, "r/apps/app:prod-current", o["ref"])
	assert.Equal(t, "r/apps/app@sha256:abc", o["pinnedRef"])
}

// "Not published here yet" is a definitive answer, so it is an empty result and
// not an error: no Application is created, and none is deleted by mistake.
func TestTagAbsentYieldsNoParams(t *testing.T) {
	f := newFakeClient()
	f.tags["r/apps/app"] = []string{"v1"}

	got, err := New(f, nil).Generate(context.Background(),
		mustCompile(t, Input{Registry: "r", Repository: "apps/app", Tag: "prod-current"}))
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 0, f.calls["Head"], "an absent tag must not be resolved")
}

// A repository that does not exist is an anomaly, not an empty result.
func TestMissingRepositoryIsAnError(t *testing.T) {
	f := newFakeClient()
	_, err := New(f, nil).Generate(context.Background(),
		mustCompile(t, Input{Registry: "r", Repository: "apps/never", Tag: "prod-current"}))
	require.Error(t, err)
}

// Any failure to obtain an answer must propagate, so the controller changes
// nothing.
func TestRegistryFailurePropagates(t *testing.T) {
	f := newFakeClient()
	f.err["TagExists"] = errors.New("registry unreachable")

	_, err := New(f, nil).Generate(context.Background(),
		mustCompile(t, Input{Registry: "r", Repository: "apps/app", Tag: "x"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registry unreachable")
}

func TestDisallowedRepository(t *testing.T) {
	f := newFakeClient()
	q := mustCompile(t, Input{Registry: "r", Repository: "other/thing", Tag: "x"})
	q.AllowRepository = func(repo string) bool { return repo == "apps/app" }

	_, err := New(f, nil).Generate(context.Background(), q)
	require.ErrorIs(t, err, ErrRepositoryNotAllowed)
	assert.Equal(t, 0, f.calls["TagExists"], "policy is enforced before any registry call")
}

func TestCompileValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		in   Input
		want string
	}{
		"no registry":         {Input{Repository: "a/b", Tag: "x"}, "registry is required"},
		"no repository":       {Input{Registry: "r", Tag: "x"}, "repository is required"},
		"no tag":              {Input{Registry: "r", Repository: "a/b"}, `tag is required for match "tag"`},
		"tag with repo match": {Input{Registry: "r", Repository: "a/b", Tag: "x", Match: MatchRepository}, `tag must be empty for match "repository"`},
		"tag with tagged":     {Input{Registry: "r", Repository: "a/b", Tag: "x", Match: MatchTagged}, `tag must be empty for match "tagged"`},
		"unknown match":       {Input{Registry: "r", Repository: "a/b", Match: "semver"}, `invalid match "semver"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.in.Compile("")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// match: repository creates an Application whenever the repository is present,
// even with no tags. The output carries no digest, because nothing was resolved.
func TestRepositoryMatch(t *testing.T) {
	t.Run("present, no tags", func(t *testing.T) {
		f := newFakeClient()
		f.tags["r/apps/app"] = nil // repository exists, empty

		got, err := New(f, nil).Generate(context.Background(),
			mustCompile(t, Input{Registry: "r", Repository: "apps/app", Match: MatchRepository}))
		require.NoError(t, err)
		require.Len(t, got, 1)

		o := got[0]["oci"].(map[string]any)
		assert.Equal(t, "apps/app", o["repository"])
		assert.Equal(t, "r/apps/app", o["ref"])
		assert.NotContains(t, o, "digest", "repository mode resolves no manifest")
		assert.NotContains(t, o, "tag")
		assert.Equal(t, 0, f.calls["TagExists"])
	})

	// An absent repository is the answer here (prune), not an error.
	t.Run("absent is empty, not an error", func(t *testing.T) {
		f := newFakeClient()
		got, err := New(f, nil).Generate(context.Background(),
			mustCompile(t, Input{Registry: "r", Repository: "apps/never", Match: MatchRepository}))
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("registry failure propagates", func(t *testing.T) {
		f := newFakeClient()
		f.err["RepositoryExists"] = errors.New("registry unreachable")
		_, err := New(f, nil).Generate(context.Background(),
			mustCompile(t, Input{Registry: "r", Repository: "apps/app", Match: MatchRepository}))
		require.Error(t, err)
	})
}

// match: tagged additionally requires at least one tag, so an empty repository
// is an empty result.
func TestTaggedMatch(t *testing.T) {
	t.Run("present with a tag", func(t *testing.T) {
		f := newFakeClient()
		f.tags["r/apps/app"] = []string{"v1"}
		got, err := New(f, nil).Generate(context.Background(),
			mustCompile(t, Input{Registry: "r", Repository: "apps/app", Match: MatchTagged}))
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("present but empty is no Application", func(t *testing.T) {
		f := newFakeClient()
		f.tags["r/apps/app"] = nil
		got, err := New(f, nil).Generate(context.Background(),
			mustCompile(t, Input{Registry: "r", Repository: "apps/app", Match: MatchTagged}))
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestRegistryDefaultApplies(t *testing.T) {
	q, err := Input{Repository: "a/b", Tag: "x"}.Compile("default.example.com")
	require.NoError(t, err)
	assert.Equal(t, "default.example.com", q.Registry)
}
