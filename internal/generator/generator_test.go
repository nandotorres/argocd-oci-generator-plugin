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
		"no registry":   {Input{Repository: "a/b", Tag: "x"}, "registry is required"},
		"no repository": {Input{Registry: "r", Tag: "x"}, "repository is required"},
		"no tag":        {Input{Registry: "r", Repository: "a/b"}, "tag is required"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.in.Compile("")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRegistryDefaultApplies(t *testing.T) {
	q, err := Input{Repository: "a/b", Tag: "x"}.Compile("default.example.com")
	require.NoError(t, err)
	assert.Equal(t, "default.example.com", q.Registry)
}
