package registry

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/oci"
)

// anonProvider always returns anonymous auth.
type anonProvider struct{}

func (anonProvider) Authenticator(context.Context, string) (authn.Authenticator, error) {
	return authn.Anonymous, nil
}

// startRegistry spins up an in-memory OCI registry and returns its host.
func startRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Host
}

func pushImage(t *testing.T, host, repo, tag string, annotations map[string]string) {
	t.Helper()
	img, err := random.Image(256, 1)
	require.NoError(t, err)
	if annotations != nil {
		img = mutate.Annotations(img, annotations).(v1.Image)
	}
	ref, err := name.NewTag(host+"/"+repo+":"+tag, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img))
}

func pushHelmArtifact(t *testing.T, host, repo, tag string) {
	t.Helper()
	img := mutate.ConfigMediaType(empty.Image, "application/vnd.cncf.helm.config.v1+json")
	ref, err := name.NewTag(host+"/"+repo+":"+tag, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img))
}

func TestClientEndToEnd(t *testing.T) {
	host := startRegistry(t)
	c := New(anonProvider{}, Options{PlainHTTP: true})
	ctx := context.Background()

	pushImage(t, host, "apps-oci/orders-api/dev", "dev-current", map[string]string{
		"org.opencontainers.image.created": "2024-05-01T12:00:00Z",
		"org.opencontainers.image.vendor":  "acme",
	})
	pushImage(t, host, "apps-oci/orders-api/dev", "v1.0.0", nil)
	pushImage(t, host, "apps-oci/checkout/prod", "v2.0.0", nil)

	t.Run("ListTags", func(t *testing.T) {
		tags, err := c.ListTags(ctx, host, "apps-oci/orders-api/dev")
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"dev-current", "v1.0.0"}, tags)
	})

	t.Run("Head", func(t *testing.T) {
		a, err := c.Head(ctx, host, "apps-oci/orders-api/dev", "dev-current")
		require.NoError(t, err)
		assert.Equal(t, "dev-current", a.Tag)
		assert.Contains(t, a.Digest, "sha256:")
		assert.Empty(t, a.Annotations, "HEAD does not fetch annotations")
	})

	t.Run("Get parses annotations and created", func(t *testing.T) {
		a, err := c.Get(ctx, host, "apps-oci/orders-api/dev", "dev-current")
		require.NoError(t, err)
		assert.Equal(t, "acme", a.Annotations["org.opencontainers.image.vendor"])
		require.NotNil(t, a.CreatedAt)
		assert.Equal(t, 2024, a.CreatedAt.Year())
	})

	t.Run("ListRepositories with prefix", func(t *testing.T) {
		repos, err := c.ListRepositories(ctx, host, "apps-oci/orders-api/")
		require.NoError(t, err)
		assert.Equal(t, []string{"apps-oci/orders-api/dev"}, repos)
	})
}

func TestGetArtifactType(t *testing.T) {
	host := startRegistry(t)
	c := New(anonProvider{}, Options{PlainHTTP: true})
	pushHelmArtifact(t, host, "charts/app", "1.0.0")

	a, err := c.Get(context.Background(), host, "charts/app", "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, "application/vnd.cncf.helm.config.v1+json", a.ArtifactType,
		"falls back to config media type when artifactType is absent")
}

// A manifest we cannot parse must be an error, not a partially-populated
// artifact: artifactType/annotations are filterable, so silently dropping them
// could turn a malformed manifest into a successful result with the artifact
// missing - which deletes Applications (DESIGN.md §2.1).
func TestEnrichFailsClosedOnBadManifest(t *testing.T) {
	art := &oci.Artifact{Registry: "r", Repository: "repo", Tag: "t"}
	err := enrich(art, []byte("{not json"))
	require.Error(t, err)
}

func TestEnrichParsesAnnotations(t *testing.T) {
	art := &oci.Artifact{}
	err := enrich(art, []byte(`{"annotations":{"org.opencontainers.image.revision":"abc"}}`))
	require.NoError(t, err)
	assert.Equal(t, "abc", art.Annotations["org.opencontainers.image.revision"])
}

// A repository that does not exist is an anomaly, not an empty result: the
// generator must fail closed rather than report zero artifacts, which would
// prune Applications.
func TestListTagsMissingRepositoryErrors(t *testing.T) {
	host := startRegistry(t)
	c := New(anonProvider{}, Options{PlainHTTP: true})

	_, err := c.ListTags(context.Background(), host, "apps-oci/never/published")
	require.Error(t, err)
}

func TestListTagsRealFailureStillErrors(t *testing.T) {
	c := New(anonProvider{}, Options{PlainHTTP: true})
	_, err := c.ListTags(context.Background(), "127.0.0.1:1", "apps-oci/x")
	require.Error(t, err, "an unreachable registry must still be an error")
}

// TagExists answers the pinned-tag question without listing the repository.
func TestTagExists(t *testing.T) {
	host := startRegistry(t)
	c := New(anonProvider{}, Options{PlainHTTP: true})
	pushImage(t, host, "apps-oci/app/dev", "v1.0.0", nil)

	t.Run("present", func(t *testing.T) {
		ok, err := c.TagExists(context.Background(), host, "apps-oci/app/dev", "v1.0.0")
		require.NoError(t, err)
		assert.True(t, ok)
	})

	// An absent tag in a repository that exists is a definitive "no".
	t.Run("absent tag, repository exists", func(t *testing.T) {
		ok, err := c.TagExists(context.Background(), host, "apps-oci/app/dev", "v9.9.9")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	// An absent repository is indistinguishable from an absent tag on a HEAD,
	// so it must be confirmed separately and surface as an error.
	t.Run("absent repository errors", func(t *testing.T) {
		_, err := c.TagExists(context.Background(), host, "apps-oci/never/published", "v1.0.0")
		require.Error(t, err)
	})

	t.Run("unreachable registry errors", func(t *testing.T) {
		_, err := c.TagExists(context.Background(), "127.0.0.1:1", "apps-oci/x", "v1")
		require.Error(t, err)
	})
}

// rawManifest publishes a pre-built manifest verbatim. It is needed because
// go-containerregistry's mutate cannot set the OCI 1.1 artifactType field, so we
// push a normal artifact (uploading its blobs) and then re-Put the same manifest
// with artifactType injected, which is what `oras push --artifact-type` writes.
type rawManifest struct {
	raw       []byte
	mediaType types.MediaType
}

func (r rawManifest) RawManifest() ([]byte, error)        { return r.raw, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return r.mediaType, nil }

func pushORASArtifact(t *testing.T, host, repo, tag, artifactType string, annotations map[string]string) {
	t.Helper()

	// Upload config + layer blobs via a normal write. The config media type is
	// deliberately NOT the artifact type, so the assertion can only pass by
	// reading the OCI 1.1 artifactType field (not the config fallback).
	img := mutate.ConfigMediaType(empty.Image, "application/vnd.oci.empty.v1+json")
	seed, err := name.NewTag(host+"/"+repo+":"+tag+"-seed", name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(seed, img))

	// Re-publish the manifest with the OCI 1.1 artifactType field set.
	raw, err := img.RawManifest()
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	m["artifactType"] = artifactType
	if annotations != nil {
		m["annotations"] = annotations
	}
	patched, err := json.Marshal(m)
	require.NoError(t, err)

	mt, err := img.MediaType()
	require.NoError(t, err)
	ref, err := name.NewTag(host+"/"+repo+":"+tag, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Put(ref, rawManifest{raw: patched, mediaType: mt}))
}

// The ORAS example (deploy/examples/applicationset-helm-oras.yaml) filters on
// artifactType plus annotations stamped at push time. enrich() prefers the OCI
// 1.1 artifactType field over the config media type, so cover that path against
// a real registry rather than only the config-media-type fallback.
func TestGetORASArtifactTypeAndAnnotations(t *testing.T) {
	host := startRegistry(t)
	c := New(anonProvider{}, Options{PlainHTTP: true})

	const helmConfig = "application/vnd.cncf.helm.config.v1+json"
	pushORASArtifact(t, host, "apps-oci/charts/orders", "1.4.2", helmConfig,
		map[string]string{"org.opencontainers.image.vendor": "payments"})

	a, err := c.Get(context.Background(), host, "apps-oci/charts/orders", "1.4.2")
	require.NoError(t, err)
	assert.Equal(t, helmConfig, a.ArtifactType, "artifactType must come from the OCI 1.1 manifest field")
	assert.Equal(t, "payments", a.Annotations["org.opencontainers.image.vendor"],
		"annotations stamped at push time must be surfaced for annotationSelectors")
}
