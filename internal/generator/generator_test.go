package generator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/oci"
)

// fakeClient is an in-memory RegistryClient for tests.
type fakeClient struct {
	repos map[string][]string     // registry -> repositories
	tags  map[string][]string     // "registry/repo" -> tags
	arts  map[string]oci.Artifact // "registry/repo:tag" -> artifact
	err   map[string]error        // method-name -> error to return
	calls map[string]int          // method-name -> count
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		repos: map[string][]string{},
		tags:  map[string][]string{},
		arts:  map[string]oci.Artifact{},
		err:   map[string]error{},
		calls: map[string]int{},
	}
}

func (f *fakeClient) ListRepositories(_ context.Context, registry, _ string) ([]string, error) {
	f.calls["ListRepositories"]++
	if e := f.err["ListRepositories"]; e != nil {
		return nil, e
	}
	return f.repos[registry], nil
}

func (f *fakeClient) ListTags(_ context.Context, registry, repo string) ([]string, error) {
	f.calls["ListTags"]++
	if e := f.err["ListTags"]; e != nil {
		return nil, e
	}
	return f.tags[registry+"/"+repo], nil
}

func (f *fakeClient) Head(_ context.Context, registry, repo, tag string) (*oci.Artifact, error) {
	f.calls["Head"]++
	if e := f.err["Head"]; e != nil {
		return nil, e
	}
	a := f.arts[registry+"/"+repo+":"+tag]
	return &a, nil
}

func (f *fakeClient) Get(_ context.Context, registry, repo, tag string) (*oci.Artifact, error) {
	f.calls["Get"]++
	if e := f.err["Get"]; e != nil {
		return nil, e
	}
	a := f.arts[registry+"/"+repo+":"+tag]
	return &a, nil
}

func (f *fakeClient) addArtifact(a oci.Artifact) {
	rr := a.Registry + "/" + a.Repository
	if !contains(f.repos[a.Registry], a.Repository) {
		f.repos[a.Registry] = append(f.repos[a.Registry], a.Repository)
	}
	if !contains(f.tags[rr], a.Tag) {
		f.tags[rr] = append(f.tags[rr], a.Tag)
	}
	f.arts[rr+":"+a.Tag] = a
}

func mustCompile(t *testing.T, in Input) *Query {
	t.Helper()
	q, err := in.Compile("")
	require.NoError(t, err)
	return q
}

// Canonical example: create an app iff a specific tag exists.
func TestExistenceCheck(t *testing.T) {
	f := newFakeClient()
	f.addArtifact(oci.Artifact{
		Registry:   "artifactory.example.com",
		Repository: "apps-oci/orders-api/orders-api/dev",
		Tag:        "dev-current",
		Digest:     "sha256:abc",
		MediaType:  "application/vnd.oci.image.manifest.v1+json",
	})
	g := New(f, nil)

	q := mustCompile(t, Input{
		Registry:   "artifactory.example.com",
		Repository: "apps-oci/orders-api/orders-api/dev",
		Tags:       []string{"dev-current"},
	})
	params, err := g.Generate(context.Background(), q)
	require.NoError(t, err)
	require.Len(t, params, 1)

	got := params[0]["oci"].(map[string]any)
	assert.Equal(t, "dev-current", got["tag"])
	assert.Equal(t, "artifactory.example.com/apps-oci/orders-api/orders-api/dev@sha256:abc", got["pinnedRef"])
	assert.Zero(t, f.calls["ListRepositories"], "no catalog call for a literal repository")
}

// Absent tag (registry reachable) => empty result, not an error.
func TestExistenceCheckAbsentTagIsEmpty(t *testing.T) {
	f := newFakeClient()
	f.tags["artifactory.example.com/apps-oci/orders-api/orders-api/dev"] = []string{"v1", "v2"}
	g := New(f, nil)

	q := mustCompile(t, Input{
		Registry:   "artifactory.example.com",
		Repository: "apps-oci/orders-api/orders-api/dev",
		Tags:       []string{"dev-current"},
	})
	params, err := g.Generate(context.Background(), q)
	require.NoError(t, err)
	assert.Empty(t, params)
}

// FailOnEmpty turns an empty result into an error (fail closed).
func TestFailOnEmpty(t *testing.T) {
	f := newFakeClient()
	f.tags["r/repo"] = []string{"v1"}
	g := New(f, nil)
	q := mustCompile(t, Input{Registry: "r", Repository: "repo", Tags: []string{"nope"}, FailOnEmpty: true})
	_, err := g.Generate(context.Background(), q)
	assert.Error(t, err)
}

// Registry failures must surface as errors (design rule #1: no deletions).
func TestListTagsErrorPropagates(t *testing.T) {
	f := newFakeClient()
	f.err["ListTags"] = errors.New("boom")
	g := New(f, nil)
	q := mustCompile(t, Input{Registry: "r", Repository: "repo"})
	_, err := g.Generate(context.Background(), q)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestCatalogErrorPropagates(t *testing.T) {
	f := newFakeClient()
	f.err["ListRepositories"] = errors.New("catalog down")
	g := New(f, nil)
	q := mustCompile(t, Input{Registry: "r", Repository: "apps/*"})
	_, err := g.Generate(context.Background(), q)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "catalog down")
}

// Wildcard repositories + tag pattern with captures.
func TestWildcardReposAndCaptures(t *testing.T) {
	f := newFakeClient()
	base := "application/vnd.oci.image.manifest.v1+json"
	for _, r := range []struct{ repo, tag string }{
		{"apps-oci/payments/x/dev", "orders-api-current"},
		{"apps-oci/checkout/y/dev", "checkout-current"},
		{"apps-oci/checkout/y/prod", "checkout-current"}, // excluded: env != dev
		{"other/thing/dev", "z-current"},                 // excluded: prefix
	} {
		f.addArtifact(oci.Artifact{Registry: "reg", Repository: r.repo, Tag: r.tag, Digest: "sha256:d", MediaType: base})
	}
	g := New(f, nil)

	q := mustCompile(t, Input{
		Registry:   "reg",
		Repository: "apps-oci/**/{env}",
		TagPattern: "{app}-current",
	})
	// Constrain env to dev via an exclude-nothing include filter is not needed;
	// the pattern already requires the last segment to be captured as {env}.
	params, err := g.Generate(context.Background(), q)
	require.NoError(t, err)

	// Keep only env==dev by post-asserting captures.
	var devs []map[string]any
	for _, p := range params {
		o := p["oci"].(map[string]any)
		caps := o["captures"].(map[string]any)
		if caps["env"] == "dev" {
			devs = append(devs, o)
		}
	}
	require.Len(t, devs, 2)

	// Assert the payments one has the right merged captures + wildcards.
	var payments map[string]any
	for _, o := range devs {
		if o["repository"] == "apps-oci/payments/x/dev" {
			payments = o
		}
	}
	require.NotNil(t, payments)
	caps := payments["captures"].(map[string]any)
	assert.Equal(t, "dev", caps["env"])
	assert.Equal(t, "orders-api", caps["app"])
	// repo wildcard "**" captured "payments/x", tag wildcard none (named only).
	assert.Equal(t, []any{"payments/x"}, payments["wildcards"])
}

func TestSemverSortDescAndLimit(t *testing.T) {
	f := newFakeClient()
	for _, tag := range []string{"v1.0.0", "v1.2.0", "v1.10.0", "v2.0.0", "latest"} {
		f.addArtifact(oci.Artifact{Registry: "reg", Repository: "app", Tag: tag, Digest: "sha256:d"})
	}
	g := New(f, nil)
	q := mustCompile(t, Input{
		Registry:   "reg",
		Repository: "app",
		TagFilters: []TagFilterSpec{{SemVer: ">= 1.0.0"}},
		Sort:       SortSemVer,
		Order:      OrderDesc,
		Limit:      2,
	})
	params, err := g.Generate(context.Background(), q)
	require.NoError(t, err)
	require.Len(t, params, 2)
	assert.Equal(t, "v2.0.0", params[0]["oci"].(map[string]any)["tag"])
	assert.Equal(t, "v1.10.0", params[1]["oci"].(map[string]any)["tag"], "semver order, not lexical")
}

func TestAnnotationSelectorAndArtifactType(t *testing.T) {
	f := newFakeClient()
	created := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	f.addArtifact(oci.Artifact{
		Registry: "reg", Repository: "charts/app", Tag: "1.0.0", Digest: "sha256:d",
		ArtifactType: "application/vnd.cncf.helm.config.v1+json",
		Annotations:  map[string]string{"org.opencontainers.image.vendor": "acme"},
		CreatedAt:    &created,
	})
	f.addArtifact(oci.Artifact{
		Registry: "reg", Repository: "charts/app", Tag: "1.0.1", Digest: "sha256:e",
		ArtifactType: "application/vnd.docker.container.image.v1+json",
		Annotations:  map[string]string{"org.opencontainers.image.vendor": "other"},
	})
	g := New(f, nil)

	q := mustCompile(t, Input{
		Registry:     "reg",
		Repository:   "charts/app",
		ArtifactType: "application/vnd.cncf.helm.config.v1+json",
		AnnotationSelectors: []AnnotationSelector{
			{Key: "org.opencontainers.image.vendor", Operator: OpIn, Values: []string{"acme"}},
		},
	})
	assert.True(t, q.NeedsManifest)
	params, err := g.Generate(context.Background(), q)
	require.NoError(t, err)
	require.Len(t, params, 1)
	o := params[0]["oci"].(map[string]any)
	assert.Equal(t, "1.0.0", o["tag"])
	assert.Equal(t, "2024-05-01T12:00:00Z", o["createdAt"])
	assert.Positive(t, f.calls["Get"], "manifest GET used when filters need it")
	assert.Zero(t, f.calls["Head"])
}
