// Package oci contains the value objects that describe an OCI artifact and how
// it is mapped into ApplicationSet generator parameters.
package oci

import (
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

func repositorySegments(repo string) []string {
	var out []string
	for _, s := range strings.Split(repo, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Annotation keys defined by the OCI image spec that we surface as first-class
// parameters when present.
const (
	AnnotationCreated  = "org.opencontainers.image.created"
	AnnotationRevision = "org.opencontainers.image.revision"
	AnnotationSource   = "org.opencontainers.image.source"
	AnnotationVersion  = "org.opencontainers.image.version"
)

// Artifact is a single resolved artifact (a tag) in a repository, enriched with
// the metadata we can cheaply obtain from its manifest.
type Artifact struct {
	// Registry is the registry host, e.g. "registry.example.com".
	Registry string
	// Repository is the repository path, e.g. "my-org/my-app".
	Repository string
	// Tag is the human-readable tag, e.g. "v1.4.2".
	Tag string
	// Digest is the content-addressable manifest digest, e.g. "sha256:...".
	Digest string
	// MediaType is the manifest media type.
	MediaType string
	// ArtifactType is the OCI 1.1 artifactType (may be empty).
	ArtifactType string
	// Annotations are the merged manifest (and config, when cheap) annotations.
	Annotations map[string]string
	// CreatedAt is parsed from the created annotation when available.
	CreatedAt *time.Time
	// SemVer is the parsed semantic version when Tag is a valid semver.
	SemVer *semver.Version
	// Captures are the merged named captures from the repository and tag patterns.
	Captures map[string]string
	// Wildcards are the anonymous (positional) captures, in pattern order:
	// repository wildcards first, then tag wildcards.
	Wildcards []string
}

// Ref returns the tag-qualified reference, e.g. "registry.example.com/my-org/my-app:v1.4.2".
func (a Artifact) Ref() string {
	return a.Registry + "/" + a.Repository + ":" + a.Tag
}

// PinnedRef returns the digest-pinned reference, which is what GitOps consumers
// should prefer for immutability.
func (a Artifact) PinnedRef() string {
	return a.Registry + "/" + a.Repository + "@" + a.Digest
}

// Params renders the artifact into a nested parameter map. The ApplicationSet
// controller flattens this with dot-style keys when goTemplate is disabled, and
// keeps it nested when goTemplate is enabled, so a single nested shape serves
// both modes (mirroring the Git generator).
func (a Artifact) Params() map[string]any {
	oci := map[string]any{
		"registry":   a.Registry,
		"repository": a.Repository,
		"tag":        a.Tag,
		"digest":     a.Digest,
		"ref":        a.Ref(),
		"pinnedRef":  a.PinnedRef(),
		"mediaType":  a.MediaType,
	}

	if a.ArtifactType != "" {
		oci["artifactType"] = a.ArtifactType
	}
	if a.CreatedAt != nil {
		oci["createdAt"] = a.CreatedAt.UTC().Format(time.RFC3339)
	}
	if len(a.Annotations) > 0 {
		annotations := make(map[string]any, len(a.Annotations))
		for k, v := range a.Annotations {
			annotations[k] = v
		}
		oci["annotations"] = annotations
	}
	if a.SemVer != nil {
		oci["semver"] = map[string]any{
			"major":      a.SemVer.Major(),
			"minor":      a.SemVer.Minor(),
			"patch":      a.SemVer.Patch(),
			"prerelease": a.SemVer.Prerelease(),
			"metadata":   a.SemVer.Metadata(),
		}
	}
	if len(a.Captures) > 0 {
		captures := make(map[string]any, len(a.Captures))
		for k, v := range a.Captures {
			captures[k] = v
		}
		oci["captures"] = captures
	}
	if len(a.Wildcards) > 0 {
		wildcards := make([]any, len(a.Wildcards))
		for i, v := range a.Wildcards {
			wildcards[i] = v
		}
		oci["wildcards"] = wildcards
	}
	if segs := repositorySegments(a.Repository); len(segs) > 0 {
		segments := make([]any, len(segs))
		for i, v := range segs {
			segments[i] = v
		}
		oci["repositorySegments"] = segments
	}

	return map[string]any{"oci": oci}
}
