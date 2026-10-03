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
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
