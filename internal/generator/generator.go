package generator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/oci"
)

// ErrRepositoryNotAllowed is returned when a request targets a repository the
// registry's allowlist does not permit. It is a caller error, so the server
// reports it as 403 rather than as an upstream failure.
var ErrRepositoryNotAllowed = errors.New("repository is not allowed")

// RegistryClient is the subset of registry behaviour the generator needs. It is
// defined here (consumer side) so the generator can be tested with a fake.
type RegistryClient interface {
	// TagExists reports whether a tag resolves. A repository that does not
	// exist is an error, not a false.
	TagExists(ctx context.Context, registry, repository, tag string) (bool, error)
	// Head resolves a tag to an artifact via a manifest HEAD.
	Head(ctx context.Context, registry, repository, tag string) (*oci.Artifact, error)
}

// Generator resolves a query against a registry.
type Generator struct {
	client RegistryClient
	log    *slog.Logger
}

// New creates a Generator.
func New(client RegistryClient, log *slog.Logger) *Generator {
	if log == nil {
		log = slog.Default()
	}
	return &Generator{client: client, log: log}
}

// Generate returns one parameter set if the artifact exists, or none if it does
// not.
//
// "Does not exist" is a definitive answer and yields an empty result, which the
// ApplicationSet controller treats as "no Application". Any failure to obtain
// an answer is an error, which it treats as "change nothing" - so a registry or
// credential problem never deletes Applications.
func (g *Generator) Generate(ctx context.Context, q *Query) ([]map[string]any, error) {
	if q.AllowRepository != nil && !q.AllowRepository(q.Repository) {
		return nil, fmt.Errorf("%w: repository %q for registry %s", ErrRepositoryNotAllowed, q.Repository, q.Registry)
	}

	exists, err := g.client.TagExists(ctx, q.Registry, q.Repository, q.Tag)
	if err != nil {
		return nil, fmt.Errorf("checking %s/%s:%s: %w", q.Registry, q.Repository, q.Tag, err)
	}
	if !exists {
		return []map[string]any{}, nil
	}

	art, err := g.client.Head(ctx, q.Registry, q.Repository, q.Tag)
	if err != nil {
		return nil, fmt.Errorf("resolving %s/%s:%s: %w", q.Registry, q.Repository, q.Tag, err)
	}
	return []map[string]any{art.Params()}, nil
}
