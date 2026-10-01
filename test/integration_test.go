// Package test contains full-stack integration tests wiring the real registry
// client, generator and HTTP server against an in-memory OCI registry.
package test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/torres/argocd-oci-generator-plugin/internal/auth"
	"github.com/torres/argocd-oci-generator-plugin/internal/config"
	"github.com/torres/argocd-oci-generator-plugin/internal/generator"
	"github.com/torres/argocd-oci-generator-plugin/internal/registry"
	"github.com/torres/argocd-oci-generator-plugin/internal/server"
)

func push(t *testing.T, host, repo, tag string, ann map[string]string) {
	t.Helper()
	img, err := random.Image(128, 1)
	require.NoError(t, err)
	if ann != nil {
		img = mutate.Annotations(img, ann).(v1.Image)
	}
	ref, err := name.NewTag(host+"/"+repo+":"+tag, name.Insecure)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img))
}

func TestFullStack(t *testing.T) {
	// 1. In-memory OCI registry.
	reg := httptest.NewServer(ggcrregistry.New())
	t.Cleanup(reg.Close)
	host := mustHost(t, reg.URL)

	// Publish two "dev-current" artifacts under a wildcard-matchable namespace.
	push(t, host, "apps-oci/orders-api/orders-api/dev", "dev-current", map[string]string{
		"org.opencontainers.image.created": "2024-05-01T12:00:00Z",
	})
	push(t, host, "apps-oci/checkout/checkout/dev", "dev-current", nil)
	push(t, host, "apps-oci/checkout/checkout/prod", "v1.0.0", nil) // should not match

	// 2. Real server stack pointed at the registry (anonymous, plain HTTP).
	cfg := &config.Config{
		Token:                 "s3cret",
		DefaultRegistry:       host,
		RequestTimeoutSeconds: 30,
		TLS:                   config.TLS{PlainHTTP: true},
		Registries:            []config.Registry{{Host: host, Auth: config.Auth{Type: config.AuthAnonymous}}},
	}
	regClient := registry.New(auth.NewResolver(cfg), registry.Options{PlainHTTP: true})
	gen := generator.New(regClient, nil)
	pluginSrv := httptest.NewServer(server.New(cfg, gen, nil).Handler())
	t.Cleanup(pluginSrv.Close)

	// 3. Call the plugin exactly as the ApplicationSet controller would.
	body := `{
      "applicationSetName": "demo",
      "input": {"parameters": {
        "repository": "apps-oci/**/{env}",
        "tagPattern": "{something}-current"
      }}
    }`
	req, err := http.NewRequest(http.MethodPost, pluginSrv.URL+"/api/v1/getparams.execute", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Output struct {
			Parameters []map[string]any `json:"parameters"`
		} `json:"output"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	// Only the two "dev" repositories with a "*-current" tag should match.
	require.Len(t, out.Output.Parameters, 2)

	byRepo := map[string]map[string]any{}
	for _, p := range out.Output.Parameters {
		o := p["oci"].(map[string]any)
		byRepo[o["repository"].(string)] = o
	}

	orders := byRepo["apps-oci/orders-api/orders-api/dev"]
	require.NotNil(t, orders)
	assert.Equal(t, "dev-current", orders["tag"])
	assert.Contains(t, orders["pinnedRef"], "@sha256:")
	caps := orders["captures"].(map[string]any)
	assert.Equal(t, "dev", caps["env"])
	assert.Equal(t, "dev", caps["something"], "tag 'dev-current' -> {something}='dev'")
	// The '**' wildcard captured the middle namespace segments.
	assert.Equal(t, []any{"orders-api/orders-api"}, orders["wildcards"])
}

func TestFullStackRegistryDownFailsClosed(t *testing.T) {
	// Point at a registry that isn't listening -> generator error -> non-2xx.
	host := "127.0.0.1:1" // unroutable port
	cfg := &config.Config{
		Token:                 "s3cret",
		DefaultRegistry:       host,
		RequestTimeoutSeconds: 5,
		TLS:                   config.TLS{PlainHTTP: true},
		Registries:            []config.Registry{{Host: host, Auth: config.Auth{Type: config.AuthAnonymous}}},
	}
	regClient := registry.New(auth.NewResolver(cfg), registry.Options{PlainHTTP: true})
	gen := generator.New(regClient, nil)
	srv := httptest.NewServer(server.New(cfg, gen, nil).Handler())
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/getparams.execute",
		bytes.NewBufferString(`{"input":{"parameters":{"repository":"apps-oci/x"}}}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.GreaterOrEqual(t, resp.StatusCode, 400, "registry failure must be a non-2xx (no deletions)")
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Host
}
