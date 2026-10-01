// Package registry implements the generator.RegistryClient interface on top of
// go-containerregistry, translating registry responses into oci.Artifact values.
package registry

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/torres/argocd-oci-generator-plugin/internal/auth"
	"github.com/torres/argocd-oci-generator-plugin/internal/oci"
)

// Client is a go-containerregistry-backed registry client.
type Client struct {
	auth      auth.Provider
	transport http.RoundTripper
	insecure  bool // use plain HTTP
}

// Options configures the Client.
type Options struct {
	// InsecureSkipVerify disables TLS certificate verification.
	InsecureSkipVerify bool
	// PlainHTTP talks to registries over http:// (for local test registries).
	PlainHTTP bool
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
	return &Client{auth: provider, transport: tr, insecure: opts.PlainHTTP}
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

// ListRepositories returns the registry catalog, narrowed to those sharing the
// literal prefix.
func (c *Client) ListRepositories(ctx context.Context, host, literalPrefix string) ([]string, error) {
	reg, err := name.NewRegistry(host, c.nameOpts()...)
	if err != nil {
		return nil, fmt.Errorf("invalid registry %q: %w", host, err)
	}
	opts, err := c.remoteOpts(ctx, host)
	if err != nil {
		return nil, err
	}
	repos, err := remote.Catalog(ctx, reg, opts...)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", host, err)
	}
	if literalPrefix == "" {
		return repos, nil
	}
	var out []string
	for _, r := range repos {
		if strings.HasPrefix(r, literalPrefix) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ListTags lists the tags of a repository.
func (c *Client) ListTags(ctx context.Context, host, repository string) ([]string, error) {
	repo, err := name.NewRepository(host+"/"+repository, c.nameOpts()...)
	if err != nil {
		return nil, fmt.Errorf("invalid repository %q: %w", repository, err)
	}
	opts, err := c.remoteOpts(ctx, host)
	if err != nil {
		return nil, err
	}
	tags, err := remote.List(repo, opts...)
	if err != nil {
		return nil, fmt.Errorf("list tags %s/%s: %w", host, repository, err)
	}
	return tags, nil
}

// Head resolves a tag to a lightweight artifact via a manifest HEAD.
func (c *Client) Head(ctx context.Context, host, repository, tag string) (*oci.Artifact, error) {
	ref, opts, err := c.reference(ctx, host, repository, tag)
	if err != nil {
		return nil, err
	}
	desc, err := remote.Head(ref, opts...)
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

// Get resolves a tag to a fully-populated artifact via a manifest GET.
func (c *Client) Get(ctx context.Context, host, repository, tag string) (*oci.Artifact, error) {
	ref, opts, err := c.reference(ctx, host, repository, tag)
	if err != nil {
		return nil, err
	}
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("get %s/%s:%s: %w", host, repository, tag, err)
	}

	art := &oci.Artifact{
		Registry:   host,
		Repository: repository,
		Tag:        tag,
		Digest:     desc.Digest.String(),
		MediaType:  string(desc.MediaType),
	}
	enrich(art, desc.Manifest)
	return art, nil
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

// manifestMeta captures the parts of a manifest we surface as parameters.
type manifestMeta struct {
	MediaType    string            `json:"mediaType"`
	ArtifactType string            `json:"artifactType"`
	Annotations  map[string]string `json:"annotations"`
	Config       struct {
		MediaType   string            `json:"mediaType"`
		Annotations map[string]string `json:"annotations"`
	} `json:"config"`
}

// enrich parses the manifest bytes and populates artifact metadata. Parsing
// failures are non-fatal: we keep the digest/media type we already have.
func enrich(art *oci.Artifact, raw []byte) {
	var m manifestMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}

	// artifactType: OCI 1.1 field if present, else the config media type, which
	// is the conventional artifact discriminator (e.g. Helm charts).
	art.ArtifactType = m.ArtifactType
	if art.ArtifactType == "" {
		art.ArtifactType = m.Config.MediaType
	}

	annotations := map[string]string{}
	for k, v := range m.Config.Annotations {
		annotations[k] = v
	}
	for k, v := range m.Annotations { // manifest annotations win
		annotations[k] = v
	}
	if len(annotations) > 0 {
		art.Annotations = annotations
	}

	if created := annotations[oci.AnnotationCreated]; created != "" {
		if t, err := time.Parse(time.RFC3339, created); err == nil {
			art.CreatedAt = &t
		}
	}
}
