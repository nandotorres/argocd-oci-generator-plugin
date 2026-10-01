package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/torres/argocd-oci-generator-plugin/internal/config"
	"github.com/torres/argocd-oci-generator-plugin/internal/generator"
)

type stubGen struct {
	params []map[string]any
	err    error
	gotQ   *generator.Query
}

func (s *stubGen) Generate(_ context.Context, q *generator.Query) ([]map[string]any, error) {
	s.gotQ = q
	return s.params, s.err
}

func newTestServer(t *testing.T, gen Generator) *httptest.Server {
	t.Helper()
	// Parse populates internal (compiled) allowlist patterns.
	parsed, err := config.Parse([]byte(`
token: s3cret
defaultRegistry: artifactory.example.com
registries:
  - host: artifactory.example.com
    auth: { type: anonymous }
    allowedRepositories: ["apps-oci/**"]
`))
	require.NoError(t, err)
	srv := httptest.NewServer(New(parsed, gen, nil).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/getparams.execute", bytes.NewBufferString(body))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t, &stubGen{})
	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestUnauthorized(t *testing.T) {
	srv := newTestServer(t, &stubGen{})
	for _, tok := range []string{"", "wrong"} {
		resp := do(t, srv, tok, `{"input":{"parameters":{"repository":"apps-oci/x"}}}`)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		resp.Body.Close()
	}
}

func TestHappyPath(t *testing.T) {
	gen := &stubGen{params: []map[string]any{
		{"oci": map[string]any{"tag": "dev-current"}},
	}}
	srv := newTestServer(t, gen)

	resp := do(t, srv, "s3cret", `{
      "applicationSetName": "demo",
      "input": {"parameters": {"repository": "apps-oci/orders-api/dev", "tags": ["dev-current"]}}
    }`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out serviceResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Output.Parameters, 1)

	// The server resolved defaults and wired the allowlist predicate.
	require.NotNil(t, gen.gotQ)
	assert.Equal(t, "artifactory.example.com", gen.gotQ.Registry)
	assert.NotNil(t, gen.gotQ.AllowRepository)
	assert.True(t, gen.gotQ.AllowRepository("apps-oci/anything"))
	assert.False(t, gen.gotQ.AllowRepository("other/thing"))
}

func TestGeneratorErrorIsBadGateway(t *testing.T) {
	srv := newTestServer(t, &stubGen{err: errors.New("registry down")})
	resp := do(t, srv, "s3cret", `{"input":{"parameters":{"repository":"apps-oci/x"}}}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	var e errorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&e))
	assert.Contains(t, e.Error, "registry down")
}

func TestInvalidInputIsBadRequest(t *testing.T) {
	srv := newTestServer(t, &stubGen{})
	// Missing required repository.
	resp := do(t, srv, "s3cret", `{"input":{"parameters":{"registry":"artifactory.example.com"}}}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUnknownFieldIsBadRequest(t *testing.T) {
	srv := newTestServer(t, &stubGen{})
	resp := do(t, srv, "s3cret", `{"input":{"parameters":{"repository":"apps-oci/x","bogus":true}}}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "typos must fail closed, not silently ignore")
}

func TestUnconfiguredRegistryIsForbidden(t *testing.T) {
	srv := newTestServer(t, &stubGen{})
	resp := do(t, srv, "s3cret", `{"input":{"parameters":{"registry":"evil.example.com","repository":"apps-oci/x"}}}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}
