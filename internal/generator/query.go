// Package generator turns a registry query into ApplicationSet parameters.
package generator

import "fmt"

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
	// Tag is the tag to resolve. Required.
	Tag string `json:"tag"`
}

// Query is a validated Input.
type Query struct {
	Registry   string
	Repository string
	Tag        string

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
	if in.Tag == "" {
		return nil, fmt.Errorf("tag is required")
	}
	return &Query{Registry: registry, Repository: in.Repository, Tag: in.Tag}, nil
}
