// Package generator turns a registry query into ApplicationSet parameters.
package generator

import "fmt"

// Match selects what existence question the generator asks. It is set
// explicitly in the ApplicationSet so the prune semantics are never implicit:
// repository and tagged modes make an absent repository an empty result (prune)
// rather than an error, the reverse of tag mode (DESIGN.md §4.3).
type Match string

const (
	// MatchTag resolves a single tag. An absent repository is an error. This is
	// the default and the only mode that yields a digest-pinned reference.
	MatchTag Match = "tag"
	// MatchRepository checks only that the repository exists, regardless of
	// whether it holds any tags. One status-only round trip.
	MatchRepository Match = "repository"
	// MatchTagged checks that the repository exists and holds at least one tag.
	MatchTagged Match = "tagged"
)

// Input is the raw, JSON-decoded shape of the ApplicationSet
// plugin.input.parameters for this generator.
//
// The generator answers exactly one question: does this artifact exist? The
// ApplicationSet controller interpolates the surrounding generator's parameters
// (cluster name, labels, ...) into these fields before we see them, so a matrix
// can vary the repository or the tag per cluster.
type Input struct {
	// Registry is the registry host. Defaults to the server's defaultRegistry.
	Registry string `json:"registry,omitempty"`
	// Repository is the repository path. Required, and literal: discovering
	// repositories needs the catalog endpoint, which is not part of the OCI
	// distribution spec and is not served by every registry.
	Repository string `json:"repository"`
	// Tag is the tag to resolve. Required for match "tag" (the default), and
	// must be empty otherwise.
	Tag string `json:"tag,omitempty"`
	// Match selects the existence question. Empty means "tag".
	Match Match `json:"match,omitempty"`
}

// Query is a validated Input.
type Query struct {
	Registry   string
	Repository string
	Tag        string
	Match      Match

	// AllowRepository, when non-nil, gates the repository against the
	// registry's allowlist.
	AllowRepository func(repo string) bool
}

// Compile validates the input and resolves the registry default.
func (in Input) Compile(defaultRegistry string) (*Query, error) {
	registry := in.Registry
	if registry == "" {
		registry = defaultRegistry
	}
	if registry == "" {
		return nil, fmt.Errorf("registry is required (none provided and no server default configured)")
	}
	if in.Repository == "" {
		return nil, fmt.Errorf("repository is required")
	}

	match := in.Match
	if match == "" {
		match = MatchTag
	}
	switch match {
	case MatchTag:
		if in.Tag == "" {
			return nil, fmt.Errorf("tag is required for match %q", MatchTag)
		}
	case MatchRepository, MatchTagged:
		if in.Tag != "" {
			return nil, fmt.Errorf("tag must be empty for match %q", match)
		}
	default:
		return nil, fmt.Errorf("invalid match %q (want %q, %q or %q)", match, MatchTag, MatchRepository, MatchTagged)
	}

	return &Query{Registry: registry, Repository: in.Repository, Tag: in.Tag, Match: match}, nil
}
