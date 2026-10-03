// Package oci contains the value object describing a resolved OCI artifact and
// how it maps into ApplicationSet generator parameters.
package oci

// Artifact is a single resolved artifact (a tag) in a repository.
type Artifact struct {
	// Registry is the registry host, e.g. "registry.example.com".
	Registry string
	// Repository is the repository path, e.g. "my-org/my-app".
	Repository string
	// Tag is the resolved tag, e.g. "production-current".
	Tag string
	// Digest is the content-addressable manifest digest, e.g. "sha256:...".
	Digest string
	// MediaType is the manifest media type.
	MediaType string
}

// Ref returns the tag-qualified reference.
func (a Artifact) Ref() string {
	return a.Registry + "/" + a.Repository + ":" + a.Tag
}

// PinnedRef returns the digest-pinned reference, which is what GitOps consumers
// should prefer for immutability.
func (a Artifact) PinnedRef() string {
	return a.Registry + "/" + a.Repository + "@" + a.Digest
}

// Params renders the artifact into a nested parameter map. The ApplicationSet
// controller flattens this with dot-style keys when goTemplate is disabled and
// keeps it nested when goTemplate is enabled, so one nested shape serves both.
func (a Artifact) Params() map[string]any {
	return map[string]any{
		"oci": map[string]any{
			"registry":   a.Registry,
			"repository": a.Repository,
			"tag":        a.Tag,
			"digest":     a.Digest,
			"ref":        a.Ref(),
			"pinnedRef":  a.PinnedRef(),
			"mediaType":  a.MediaType,
		},
	}
}
