// Package generator turns registry queries into ApplicationSet parameters:
// it lists artifacts, applies tag filters, sorts, and limits the result set.
package generator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/oci"
	"github.com/nandotorres/argocd-oci-generator-plugin/internal/pattern"
)

// ErrRepositoryNotAllowed is returned when a request targets a repository that
// the registry's allowlist does not permit. It is a caller (policy) error, so
// the server reports it as 403 rather than as an upstream failure.
var ErrRepositoryNotAllowed = errors.New("repository is not allowed")

// RegistryClient is the subset of registry behaviour the generator needs. It is
// defined here (consumer side) so the generator can be tested with a fake and
// stays decoupled from go-containerregistry.
type RegistryClient interface {
	// ListRepositories enumerates repositories in a registry, optionally
	// narrowed by a literal prefix. Used only for wildcard repository patterns.
	ListRepositories(ctx context.Context, registry, literalPrefix string) ([]string, error)
	// ListTags lists the tags of a repository.
	ListTags(ctx context.Context, registry, repository string) ([]string, error)
	// Head resolves a tag to a lightweight artifact (digest + media type) using
	// a manifest HEAD.
	Head(ctx context.Context, registry, repository, tag string) (*oci.Artifact, error)

	// TagExists reports whether a single tag resolves, without listing the
	// repository. A repository that does not exist is an error, not a false.
	TagExists(ctx context.Context, registry, repository, tag string) (bool, error)
	// Get resolves a tag to a fully-populated artifact (annotations, artifact
	// type, created time) using a manifest GET.
	Get(ctx context.Context, registry, repository, tag string) (*oci.Artifact, error)
}

// Generator turns a validated Query into ApplicationSet parameter sets.
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

// Generate executes the query and returns one parameter map per matched
// artifact. Any upstream/registry failure returns an error so the ApplicationSet
// controller fails closed and never deletes applications.
func (g *Generator) Generate(ctx context.Context, q *Query) ([]map[string]any, error) {
	repos, repoCaptures, err := g.resolveRepositories(ctx, q)
	if err != nil {
		return nil, err
	}

	var artifacts []oci.Artifact
	for _, repo := range repos {
		arts, err := g.artifactsForRepo(ctx, q, repo, repoCaptures[repo])
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, arts...)
	}

	artifacts = q.sortAndLimit(artifacts)

	if len(artifacts) == 0 && q.FailOnEmpty {
		return nil, fmt.Errorf("no artifacts matched and failOnEmpty is set (registry=%s repository=%s)", q.Registry, q.RepoPattern.Raw())
	}

	params := make([]map[string]any, 0, len(artifacts))
	for i := range artifacts {
		params = append(params, artifacts[i].Params())
	}
	return params, nil
}

// resolveRepositories returns the concrete repositories to query and, for each,
// the named/anonymous captures derived from the repository pattern.
func (g *Generator) resolveRepositories(ctx context.Context, q *Query) ([]string, map[string]repoMatch, error) {
	captures := map[string]repoMatch{}

	if !q.RepoPattern.HasWildcard() {
		repo := q.RepoPattern.Raw()
		if q.AllowRepository != nil && !q.AllowRepository(repo) {
			return nil, nil, fmt.Errorf("%w: repository %q for registry %s", ErrRepositoryNotAllowed, repo, q.Registry)
		}
		captures[repo] = repoMatch{}
		return []string{repo}, captures, nil
	}

	all, err := g.client.ListRepositories(ctx, q.Registry, q.RepoPattern.LiteralPrefix())
	if err != nil {
		return nil, nil, fmt.Errorf("listing repositories in %s: %w", q.Registry, err)
	}

	var matched []string
	for _, repo := range all {
		named, ordered, ok := q.RepoPattern.Match(repo)
		if !ok {
			continue
		}
		if q.AllowRepository != nil && !q.AllowRepository(repo) {
			continue
		}
		matched = append(matched, repo)
		captures[repo] = repoMatch{named: named, wildcards: pattern.Anonymous(ordered)}
	}
	sort.Strings(matched)

	g.log.Debug("resolved repositories",
		slog.String("registry", q.Registry),
		slog.String("pattern", q.RepoPattern.Raw()),
		slog.Int("catalogSize", len(all)),
		slog.Int("matched", len(matched)),
	)
	return matched, captures, nil
}

// candidateTags returns the tags worth resolving for this repository.
//
// A pinned tag is already the entire candidate set, so we ask the registry
// about that one tag instead of listing a repository whose tag count grows with
// every release ever published. Everything else needs the full list, because
// patterns, filters and ordering are defined over the whole tag space.
func (g *Generator) candidateTags(ctx context.Context, q *Query, repo string) ([]string, error) {
	if q.ExactTag == "" {
		tags, err := g.client.ListTags(ctx, q.Registry, repo)
		if err != nil {
			return nil, fmt.Errorf("listing tags for %s/%s: %w", q.Registry, repo, err)
		}
		return tags, nil
	}

	exists, err := g.client.TagExists(ctx, q.Registry, repo, q.ExactTag)
	if err != nil {
		return nil, fmt.Errorf("checking %s/%s:%s: %w", q.Registry, repo, q.ExactTag, err)
	}
	if !exists {
		return nil, nil
	}
	return []string{q.ExactTag}, nil
}

type repoMatch struct {
	named     map[string]string
	wildcards []string
}

func (g *Generator) artifactsForRepo(ctx context.Context, q *Query, repo string, rm repoMatch) ([]oci.Artifact, error) {
	tags, err := g.candidateTags(ctx, q, repo)
	if err != nil {
		return nil, err
	}

	var out []oci.Artifact
	for _, tag := range tags {
		// Match the tag pattern once and reuse its captures; the remaining
		// tag-level predicates are checked separately.
		tagNamed, tagOrdered := map[string]string{}, []string(nil)
		if q.TagPattern != nil {
			named, ordered, ok := q.TagPattern.Match(tag)
			if !ok {
				continue
			}
			tagNamed = named
			tagOrdered = pattern.Anonymous(ordered)
		}
		if !q.tagSelected(tag) {
			continue
		}

		var art *oci.Artifact
		if q.NeedsManifest {
			art, err = g.client.Get(ctx, q.Registry, repo, tag)
		} else {
			art, err = g.client.Head(ctx, q.Registry, repo, tag)
		}
		if err != nil {
			return nil, fmt.Errorf("resolving %s/%s:%s: %w", q.Registry, repo, tag, err)
		}

		if q.ArtifactType != "" && art.ArtifactType != q.ArtifactType {
			continue
		}
		if !q.matchAnnotations(art) {
			continue
		}

		art.Captures = mergeCaptures(rm.named, tagNamed)
		art.Wildcards = append(append([]string{}, rm.wildcards...), tagOrdered...)
		art.SemVer = parseSemVer(tag)
		out = append(out, *art)
	}
	return out, nil
}

func mergeCaptures(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// FullReference is a small helper for logging/debugging.
func FullReference(registry, repo, tag string) string {
	return strings.Join([]string{registry, repo}, "/") + ":" + tag
}
