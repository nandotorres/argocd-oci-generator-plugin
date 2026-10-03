// Package registry implements the generator.RegistryClient interface on top of
// go-containerregistry, translating registry responses into oci.Artifact values.
package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/auth"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/metrics"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/oci"
)

// Client is a go-containerregistry-backed registry client.
type Client struct {
	auth      auth.Provider
	transport http.RoundTripper
	insecure  bool // use plain HTTP
	metrics   *metrics.Metrics
}

// Options configures the Client.
type Options struct {
	// InsecureSkipVerify disables TLS certificate verification.
	InsecureSkipVerify bool
	// PlainHTTP talks to registries over http:// (for local test registries).
	PlainHTTP bool
	// Metrics, when set, records upstream call counts and latency. Optional.
	Metrics *metrics.Metrics
}

// New creates a registry Client.
func New(provider auth.Provider, opts Options) *Client {
	tr := remote.DefaultTransport.(*http.Transport).Clone()
	if opts.InsecureSkipVerify {
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		}
		tr.TLSClientConfig.InsecureSkipVerify = true
	}
	return &Client{auth: provider, transport: tr, insecure: opts.PlainHTTP, metrics: opts.Metrics}
}

// observe records one upstream call. Latency is measured here rather than in
// the generator so that registry time can be told apart from our own.
func (c *Client) observe(operation string, start time.Time, err error) {
	c.metrics.ObserveRegistryCall(operation, err, time.Since(start))
}

func (c *Client) nameOpts() []name.Option {
	if c.insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

func (c *Client) remoteOpts(ctx context.Context, host string) ([]remote.Option, error) {
	authr, err := c.auth.Authenticator(ctx, host)
	if err != nil {
		return nil, err
	}
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuth(authr),
		remote.WithTransport(c.transport),
	}, nil
}

// TagExists reports whether a single tag resolves, using a manifest HEAD rather
// than listing the repository.
//
// A 404 here is ambiguous: a HEAD carries no body, and registries are
// inconsistent about whether an absent tag in an absent repository reports
// MANIFEST_UNKNOWN or NAME_UNKNOWN. So on a miss we confirm the repository
// separately, which keeps "repository does not exist" an error rather than
// silently reporting the tag as absent.
func (c *Client) TagExists(ctx context.Context, host, repository, tag string) (bool, error) {
	ref, opts, err := c.reference(ctx, host, repository, tag)
	if err != nil {
		return false, err
	}

	start := time.Now()
	_, err = remote.Head(ref, opts...)
	c.observe("head", start, err)
	if err == nil {
		return true, nil
	}

	var terr *transport.Error
	if !errors.As(err, &terr) || terr.StatusCode != http.StatusNotFound {
		return false, err // auth, 5xx, network: fail closed
	}

	// The tag is absent; the repository may be too.
	start = time.Now()
	exists, err := c.repositoryExists(ctx, host, repository)
	c.observe("list_tags", start, err)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil // repository is there, the tag simply is not
	}
	return false, fmt.Errorf("list tags %s/%s: repository does not exist", host, repository)
}

// repositoryExists probes the tag-list endpoint and reads only the status code.
//
// remote.List cannot be used here: it pages through every tag in the
// repository, so asking it a yes/no question costs one request per page.
func (c *Client) repositoryExists(ctx context.Context, host, repository string) (bool, error) {
	repo, err := name.NewRepository(host+"/"+repository, c.nameOpts()...)
	if err != nil {
		return false, fmt.Errorf("invalid repository %q: %w", repository, err)
	}
	authr, err := c.auth.Authenticator(ctx, host)
	if err != nil {
		return false, err
	}
	rt, err := transport.NewWithContext(ctx, repo.Registry, authr, c.transport, []string{repo.Scope("pull")})
	if err != nil {
		return false, fmt.Errorf("authenticating to %s: %w", host, err)
	}

	u := url.URL{
		Scheme:   repo.Scheme(),
		Host:     repo.RegistryStr(),
		Path:     "/v2/" + repo.RepositoryStr() + "/tags/list",
		RawQuery: "n=1",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return false, fmt.Errorf("probing %s/%s: %w", host, repository, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	default:
		return false, fmt.Errorf("probing %s/%s: unexpected status %s", host, repository, resp.Status)
	}
}

// Head resolves a tag to a lightweight artifact via a manifest HEAD.
func (c *Client) Head(ctx context.Context, host, repository, tag string) (*oci.Artifact, error) {
	ref, opts, err := c.reference(ctx, host, repository, tag)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	desc, err := remote.Head(ref, opts...)
	c.observe("head", start, err)
	if err != nil {
		return nil, fmt.Errorf("head %s/%s:%s: %w", host, repository, tag, err)
	}
	return &oci.Artifact{
		Registry:   host,
		Repository: repository,
		Tag:        tag,
		Digest:     desc.Digest.String(),
		MediaType:  string(desc.MediaType),
	}, nil
}

func (c *Client) reference(ctx context.Context, host, repository, tag string) (name.Reference, []remote.Option, error) {
	ref, err := name.NewTag(host+"/"+repository+":"+tag, c.nameOpts()...)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid reference %s/%s:%s: %w", host, repository, tag, err)
	}
	opts, err := c.remoteOpts(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	return ref, opts, nil
}
