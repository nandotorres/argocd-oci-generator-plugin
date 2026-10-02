package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/config"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/generator"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/metrics"
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
defaultRegistry: registry.example.com
registries:
  - host: registry.example.com
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
	assert.Equal(t, "registry.example.com", gen.gotQ.Registry)
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
	resp := do(t, srv, "s3cret", `{"input":{"parameters":{"registry":"registry.example.com"}}}`)
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

// A policy denial is the caller's fault: report 403, not a generic 502. Both
// are fail-closed (no deletions); this is about diagnosability.
func TestDisallowedRepositoryReturns403(t *testing.T) {
	gen := &stubGen{err: fmt.Errorf("%w: repository %q for registry x",
		generator.ErrRepositoryNotAllowed, "other/thing")}
	srv := newTestServer(t, gen)

	resp := do(t, srv, "s3cret", `{"input":{"parameters":{"repository":"other/thing"}}}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// Upstream/registry failures stay 502.
func TestUpstreamFailureReturns502(t *testing.T) {
	gen := &stubGen{err: errors.New("registry unreachable")}
	srv := newTestServer(t, gen)

	resp := do(t, srv, "s3cret", `{"input":{"parameters":{"repository":"apps-oci/a"}}}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// An oversized body must be rejected rather than allocated.
func TestOversizedBodyRejected(t *testing.T) {
	srv := newTestServer(t, &stubGen{})
	huge := `{"applicationSetName":"` + strings.Repeat("a", maxRequestBodyBytes+1) + `"}`

	resp := do(t, srv, "s3cret", huge)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestMetricsEndpointAndCounters(t *testing.T) {
	mx := metrics.New()
	gen := &stubGen{params: []map[string]any{{"oci": map[string]any{"tag": "v1"}}}}
	parsed, err := config.Parse([]byte("token: s3cret\ndefaultRegistry: registry.example.com\nregistries:\n  - host: registry.example.com\n    auth: { type: anonymous }\n"))
	require.NoError(t, err)
	srv := httptest.NewServer(New(parsed, gen, nil, WithMetrics(mx)).Handler())
	t.Cleanup(srv.Close)

	// One success and one auth failure, so both code paths are recorded.
	do(t, srv, "s3cret", `{"input":{"parameters":{"repository":"a/b"}}}`).Body.Close()
	do(t, srv, "wrong", `{"input":{"parameters":{"repository":"a/b"}}}`).Body.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := string(body)

	assert.Contains(t, out, `ocigen_getparams_requests_total{code="200"} 1`)
	assert.Contains(t, out, `ocigen_getparams_requests_total{code="401"} 1`)
	assert.Contains(t, out, "ocigen_getparams_duration_seconds")
	assert.Contains(t, out, "ocigen_generated_parameters")
}

// Without the option there is no /metrics endpoint and no instrumentation.
func TestMetricsDisabledByDefault(t *testing.T) {
	srv := newTestServer(t, &stubGen{})
	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
