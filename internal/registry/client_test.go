package registry

import (
	"context"
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
