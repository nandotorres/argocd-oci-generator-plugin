package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/config"
)

const ecrConfigYAML = `
token: x
registries:
  - host: 111122223333.dkr.ecr.us-east-1.amazonaws.com
    auth: { type: ecr, region: us-east-1, roleArn: "arn:aws:iam::111122223333:role/reader" }
`

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
  - host: registry.example.com
    auth: { type: basic, username: "${U}", password: "${P}" }
`)
	r := NewResolver(c)
	a, err := r.Authenticator(context.Background(), "registry.example.com")
	require.NoError(t, err)
	cfg := authConfig(t, a)
	assert.Equal(t, "svc", cfg.Username)
	assert.Equal(t, "pw", cfg.Password)
}

func TestECRTokenCachingAndRefresh(t *testing.T) {
	r := NewResolver(mustConfig(t, ecrConfigYAML))

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

// A refresh that fails inside the grace window must NOT discard a cached token
// that is still valid: we refresh early precisely so a transient STS/ECR blip
// does not turn into a generator error (and a stalled ApplicationSet).
func TestECRRefreshFailureKeepsValidToken(t *testing.T) {
	r := NewResolver(mustConfig(t, ecrConfigYAML))

	now := time.Now()
	r.ecr.now = func() time.Time { return now }

	var calls int
	r.ecr.fetch = func(_ context.Context, _, _ string) (ecrToken, error) {
		calls++
		if calls == 1 {
			return ecrToken{username: "AWS", password: "good", expiresAt: now.Add(12 * time.Hour)}, nil
		}
		return ecrToken{}, errors.New("sts unavailable")
	}

	host := "111122223333.dkr.ecr.us-east-1.amazonaws.com"
	a, err := r.Authenticator(context.Background(), host)
	require.NoError(t, err)
	cfg, err := a.Authorization()
	require.NoError(t, err)
	assert.Equal(t, "good", cfg.Password)

	// Move into the refresh margin: the token is near expiry but still valid.
	now = now.Add(12*time.Hour - ecrRefreshMargin + time.Minute)

	a, err = r.Authenticator(context.Background(), host)
	require.NoError(t, err, "refresh failure must not fail the request while the token is valid")
	cfg, err = a.Authorization()
	require.NoError(t, err)
	assert.Equal(t, "good", cfg.Password, "should keep serving the cached token")
	assert.Equal(t, 2, calls, "a refresh should have been attempted")
}

// Once the cached token has actually expired, a failing refresh must surface as
// an error (fail closed) rather than handing out a dead credential.
func TestECRRefreshFailureAfterExpiryErrors(t *testing.T) {
	r := NewResolver(mustConfig(t, ecrConfigYAML))

	now := time.Now()
	r.ecr.now = func() time.Time { return now }

	var calls int
	r.ecr.fetch = func(_ context.Context, _, _ string) (ecrToken, error) {
		calls++
		if calls == 1 {
			return ecrToken{username: "AWS", password: "good", expiresAt: now.Add(1 * time.Hour)}, nil
		}
		return ecrToken{}, errors.New("sts unavailable")
	}

	host := "111122223333.dkr.ecr.us-east-1.amazonaws.com"
	_, err := r.Authenticator(context.Background(), host)
	require.NoError(t, err)

	now = now.Add(2 * time.Hour) // past expiry
	_, err = r.Authenticator(context.Background(), host)
	require.Error(t, err, "an expired token with a failing refresh must fail closed")
	assert.Contains(t, err.Error(), "obtaining ECR token")
}
