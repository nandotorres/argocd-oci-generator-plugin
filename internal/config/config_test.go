package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAndExpand(t *testing.T) {
	t.Setenv("PLUGIN_TOKEN", "s3cret")
	t.Setenv("ART_USER", "svc")
	t.Setenv("ART_PASS", "pw")

	raw := []byte(`
token: ${PLUGIN_TOKEN}
defaultRegistry: registry.example.com
registries:
  - host: registry.example.com
    auth:
      type: basic
      username: ${ART_USER}
      password: ${ART_PASS}
    allowedRepositories: ["apps-oci/**"]
  - host: 111122223333.dkr.ecr.us-east-1.amazonaws.com
    auth:
      type: ecr
      region: us-east-1
      roleArn: arn:aws:iam::111122223333:role/reader
`)
	c, err := Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, ":8080", c.Listen)
	assert.Equal(t, 60, c.RequestTimeoutSeconds)
	assert.Equal(t, "s3cret", c.Token)

	art := c.RegistryFor("registry.example.com")
	require.NotNil(t, art)
	assert.Equal(t, "svc", art.Auth.Username)
	assert.Equal(t, "pw", art.Auth.Password)
	assert.True(t, art.RepositoryAllowed("apps-oci/orders/dev"))
	assert.False(t, art.RepositoryAllowed("other/thing"))

	assert.Nil(t, c.RegistryFor("unknown.example.com"))
}

func TestMissingEnvFails(t *testing.T) {
	_, err := Parse([]byte(`token: ${DOES_NOT_EXIST}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DOES_NOT_EXIST")
}

func TestTokenRequired(t *testing.T) {
	_, err := Parse([]byte(`registries: []`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token is required")
}

func TestBasicRequiresCreds(t *testing.T) {
	_, err := Parse([]byte(`
token: x
registries:
  - host: r.example.com
    auth: { type: basic }
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "basic auth requires")
}

func TestDuplicateHostRejected(t *testing.T) {
	_, err := Parse([]byte(`
token: x
registries:
  - host: r.example.com
    auth: { type: anonymous }
  - host: r.example.com
    auth: { type: anonymous }
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate host")
}

func TestEmptyAllowlistPermitsAll(t *testing.T) {
	c, err := Parse([]byte(`
token: x
registries:
  - host: r.example.com
    auth: { type: anonymous }
`))
	require.NoError(t, err)
	assert.True(t, c.RegistryFor("r.example.com").RepositoryAllowed("anything/at/all"))
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Parse([]byte(`token: x
bogusField: true`))
	require.Error(t, err)
}
