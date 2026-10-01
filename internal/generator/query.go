package generator

import (
	"fmt"
	"regexp"

	"github.com/Masterminds/semver/v3"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/pattern"
)

// SortKey selects how matched artifacts are ordered before limiting.
type SortKey string

// Supported sort keys.
const (
	SortSemVer  SortKey = "semver"
	SortAlpha   SortKey = "alpha"
	SortCreated SortKey = "created"
)

// SortOrder is ascending or descending.
type SortOrder string

// Supported sort orders.
const (
	OrderAsc  SortOrder = "asc"
	OrderDesc SortOrder = "desc"
)

// SelectorOperator is a Kubernetes-style set-based operator for annotations.
type SelectorOperator string

// Supported set-based selector operators.
const (
	OpIn           SelectorOperator = "In"
	OpNotIn        SelectorOperator = "NotIn"
	OpExists       SelectorOperator = "Exists"
	OpDoesNotExist SelectorOperator = "DoesNotExist"
)

// TagFilterSpec is a single tag filter. Exactly one of Regex or SemVer must be set.
type TagFilterSpec struct {
	Regex  string `json:"regex,omitempty"`
	SemVer string `json:"semver,omitempty"`
}

// AnnotationSelector matches against an artifact's annotations.
type AnnotationSelector struct {
	Key      string           `json:"key"`
	Operator SelectorOperator `json:"operator"`
	Values   []string         `json:"values,omitempty"`
}

// Input is the raw, JSON-decoded shape of the ApplicationSet
// plugin.input.parameters for this generator.
type Input struct {
	Registry            string               `json:"registry,omitempty"`
	Repository          string               `json:"repository"`
	TagPattern          string               `json:"tagPattern,omitempty"`
	Tags                []string             `json:"tags,omitempty"`
	TagFilters          []TagFilterSpec      `json:"tagFilters,omitempty"`
	ExcludeTagFilters   []TagFilterSpec      `json:"excludeTagFilters,omitempty"`
	ArtifactType        string               `json:"artifactType,omitempty"`
	AnnotationSelectors []AnnotationSelector `json:"annotationSelectors,omitempty"`
	Sort                SortKey              `json:"sort,omitempty"`
	Order               SortOrder            `json:"order,omitempty"`
	Limit               int                  `json:"limit,omitempty"`
	// FailOnEmpty makes an otherwise-valid empty result an error, so that
	// applications are never deleted just because a repo transiently has no
	// matching tags. Off by default (empty means "no apps", which is valid).
	FailOnEmpty bool `json:"failOnEmpty,omitempty"`
}

// compiledTagFilter is a validated tag filter.
type compiledTagFilter struct {
	raw    TagFilterSpec
	regex  *regexp.Regexp
	semver *semver.Constraints
}

func (f compiledTagFilter) matches(tag string, v *semver.Version) bool {
	switch {
	case f.regex != nil:
		return f.regex.MatchString(tag)
	case f.semver != nil:
		return v != nil && f.semver.Check(v)
	default:
		return false
	}
}

// Query is the validated, compiled form of an Input, ready to execute.
type Query struct {
	Registry     string
	RepoPattern  *pattern.Pattern
	TagPattern   *pattern.Pattern // nil when not set
	ExactTags    map[string]bool  // nil when not set
	Includes     []compiledTagFilter
	Excludes     []compiledTagFilter
	ArtifactType string
	Selectors    []AnnotationSelector
	Sort         SortKey
	Order        SortOrder
	Limit        int
	FailOnEmpty  bool
	// AllowRepository, when non-nil, gates every concrete repository. A literal
	// repository that is denied yields an error; denied catalog matches are
	// filtered out. Set by the server from the registry allowlist.
	AllowRepository func(repo string) bool
	// NeedsManifest is true when we must fetch each candidate's manifest to
	// evaluate the query (artifactType/annotation filters). Digest is always
	// resolved regardless, for digest-pinned refs.
	NeedsManifest bool
}

func compileTagFilters(specs []TagFilterSpec) ([]compiledTagFilter, error) {
	out := make([]compiledTagFilter, 0, len(specs))
	for i, s := range specs {
		switch {
		case s.Regex != "" && s.SemVer != "":
			return nil, fmt.Errorf("tag filter %d: set only one of regex or semver", i)
		case s.Regex != "":
			re, err := regexp.Compile(s.Regex)
			if err != nil {
				return nil, fmt.Errorf("tag filter %d: invalid regex %q: %w", i, s.Regex, err)
			}
			out = append(out, compiledTagFilter{raw: s, regex: re})
		case s.SemVer != "":
			c, err := semver.NewConstraint(s.SemVer)
			if err != nil {
				return nil, fmt.Errorf("tag filter %d: invalid semver constraint %q: %w", i, s.SemVer, err)
			}
			out = append(out, compiledTagFilter{raw: s, semver: c})
		default:
			return nil, fmt.Errorf("tag filter %d: set one of regex or semver", i)
		}
	}
	return out, nil
}

// Compile validates the input and returns an executable Query. Any validation
// failure returns an error so the generator fails closed (no deletions).
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

	repoPat, err := pattern.Compile(in.Repository, pattern.ModePath)
	if err != nil {
		return nil, fmt.Errorf("invalid repository pattern: %w", err)
	}

	q := &Query{
		Registry:     registry,
		RepoPattern:  repoPat,
		ArtifactType: in.ArtifactType,
		Selectors:    in.AnnotationSelectors,
		Sort:         in.Sort,
		Order:        in.Order,
		Limit:        in.Limit,
		FailOnEmpty:  in.FailOnEmpty,
	}

	captureNames := map[string]bool{}
	for _, n := range repoPat.CaptureNames() {
		captureNames[n] = true
	}

	if in.TagPattern != "" {
		tp, err := pattern.Compile(in.TagPattern, pattern.ModeTag)
		if err != nil {
			return nil, fmt.Errorf("invalid tagPattern: %w", err)
		}
		for _, n := range tp.CaptureNames() {
			if captureNames[n] {
				return nil, fmt.Errorf("duplicate capture name %q across repository and tag patterns", n)
			}
			captureNames[n] = true
		}
		q.TagPattern = tp
	}

	if len(in.Tags) > 0 {
		q.ExactTags = make(map[string]bool, len(in.Tags))
		for _, t := range in.Tags {
			if t == "" {
				return nil, fmt.Errorf("tags must not contain empty strings")
			}
			q.ExactTags[t] = true
		}
	}

	if q.Includes, err = compileTagFilters(in.TagFilters); err != nil {
		return nil, err
	}
	if q.Excludes, err = compileTagFilters(in.ExcludeTagFilters); err != nil {
		return nil, err
	}

	for i, s := range in.AnnotationSelectors {
		if s.Key == "" {
			return nil, fmt.Errorf("annotationSelector %d: key is required", i)
		}
		switch s.Operator {
		case OpIn, OpNotIn:
			if len(s.Values) == 0 {
				return nil, fmt.Errorf("annotationSelector %d: operator %s requires values", i, s.Operator)
			}
		case OpExists, OpDoesNotExist:
			if len(s.Values) != 0 {
				return nil, fmt.Errorf("annotationSelector %d: operator %s must not have values", i, s.Operator)
			}
		default:
			return nil, fmt.Errorf("annotationSelector %d: invalid operator %q", i, s.Operator)
		}
	}

	switch q.Sort {
	case "", SortSemVer, SortAlpha, SortCreated:
	default:
		return nil, fmt.Errorf("invalid sort %q (want semver|alpha|created)", q.Sort)
	}
	switch q.Order {
	case "", OrderAsc, OrderDesc:
	default:
		return nil, fmt.Errorf("invalid order %q (want asc|desc)", q.Order)
	}
	if q.Limit < 0 {
		return nil, fmt.Errorf("limit must be >= 0")
	}

	q.NeedsManifest = q.ArtifactType != "" || len(q.Selectors) > 0 || q.Sort == SortCreated
	return q, nil
}
