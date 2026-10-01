package auth

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/config"
)

func mustConfig(t *testing.T, raw string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(raw))
	require.NoError(t, err)
	return c
}

func authConfig(t *testing.T, a authn.Authenticator) authn.AuthConfig {
	t.Helper()
	cfg, err := a.Authorization()
	require.NoError(t, err)
	return *cfg
}

func TestAnonymousAndUnknownHost(t *testing.T) {
	c := mustConfig(t, `
token: x
registries:
  - host: anon.example.com
    auth: { type: anonymous }
`)
	r := NewResolver(c)

	a, err := r.Authenticator(context.Background(), "anon.example.com")
	require.NoError(t, err)
	assert.Equal(t, authn.Anonymous, a)

	a, err = r.Authenticator(context.Background(), "unknown.example.com")
	require.NoError(t, err)
	assert.Equal(t, authn.Anonymous, a, "unknown hosts default to anonymous")
}

func TestBasicAuthenticator(t *testing.T) {
	t.Setenv("U", "svc")
	t.Setenv("P", "pw")
	c := mustConfig(t, `
token: x
registries:
  - host: artifactory.example.com
    auth: { type: basic, username: "${U}", password: "${P}" }
`)
	r := NewResolver(c)
	a, err := r.Authenticator(context.Background(), "artifactory.example.com")
	require.NoError(t, err)
	cfg := authConfig(t, a)
	assert.Equal(t, "svc", cfg.Username)
	assert.Equal(t, "pw", cfg.Password)
}

func TestECRTokenCachingAndRefresh(t *testing.T) {
	c := mustConfig(t, `
token: x
registries:
  - host: 111122223333.dkr.ecr.us-east-1.amazonaws.com
    auth: { type: ecr, region: us-east-1, roleArn: "arn:aws:iam::111122223333:role/reader" }
`)
	r := NewResolver(c)

	now := time.Now()
	r.ecr.now = func() time.Time { return now }
	calls := 0
	r.ecr.fetch = func(_ context.Context, region, role string) (ecrToken, error) {
		calls++
		assert.Equal(t, "us-east-1", region)
		assert.Equal(t, "arn:aws:iam::111122223333:role/reader", role)
		return ecrToken{username: "AWS", password: "tok", expiresAt: now.Add(12 * time.Hour)}, nil
	}

	host := "111122223333.dkr.ecr.us-east-1.amazonaws.com"
	a, err := r.Authenticator(context.Background(), host)
	require.NoError(t, err)
	assert.Equal(t, "tok", authConfig(t, a).Password)
	assert.Equal(t, 1, calls)

	// Cached: no new fetch.
	_, err = r.Authenticator(context.Background(), host)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)

	// Advance past refresh margin: re-fetch.
	now = now.Add(12 * time.Hour)
	_, err = r.Authenticator(context.Background(), host)
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
}

func TestDecodeAuthToken(t *testing.T) {
	tok := base64.StdEncoding.EncodeToString([]byte("AWS:secretpw"))
	u, p, err := decodeAuthToken(tok)
	require.NoError(t, err)
	assert.Equal(t, "AWS", u)
	assert.Equal(t, "secretpw", p)

	_, _, err = decodeAuthToken("not-base64!!!")
	assert.Error(t, err)
}
