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
	// RepositoryExists reports whether the repository exists, regardless of
	// tags. A definitive absence is (false, nil), not an error.
	RepositoryExists(ctx context.Context, registry, repository string) (bool, error)
	// RepositoryHasTags reports whether the repository exists and holds at
	// least one tag. An existing but empty repository is (false, nil).
	RepositoryHasTags(ctx context.Context, registry, repository string) (bool, error)
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

	switch q.Match {
	case MatchRepository, MatchTagged:
		return g.generateRepository(ctx, q)
	default:
		return g.generateTag(ctx, q)
	}
}

// generateTag resolves a single pinned tag (the default). An absent repository
// is an error, so a registry glitch never deletes Applications.
func (g *Generator) generateTag(ctx context.Context, q *Query) ([]map[string]any, error) {
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

// generateRepository answers an existence-only question. Here an absent
// repository is the answer (empty result, prune), not an anomaly: the caller
// opted into that by setting match explicitly. The output carries no digest,
// because no manifest was resolved.
func (g *Generator) generateRepository(ctx context.Context, q *Query) ([]map[string]any, error) {
	var (
		exists bool
		err    error
	)
	if q.Match == MatchTagged {
		exists, err = g.client.RepositoryHasTags(ctx, q.Registry, q.Repository)
	} else {
		exists, err = g.client.RepositoryExists(ctx, q.Registry, q.Repository)
	}
	if err != nil {
		return nil, fmt.Errorf("checking %s/%s (match %s): %w", q.Registry, q.Repository, q.Match, err)
	}
	if !exists {
		return []map[string]any{}, nil
	}
	repo := oci.Repository{Registry: q.Registry, Repository: q.Repository}
	return []map[string]any{repo.Params()}, nil
}
