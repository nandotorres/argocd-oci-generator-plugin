package generator

import (
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/nandotorres/argocd-oci-generator-plugin/internal/oci"
)

// matchAnnotations reports whether the artifact's annotations satisfy every
// selector (logical AND).
func (q *Query) matchAnnotations(a *oci.Artifact) bool {
	for _, s := range q.Selectors {
		val, present := a.Annotations[s.Key]
		switch s.Operator {
		case OpExists:
			if !present {
				return false
			}
		case OpDoesNotExist:
			if present {
				return false
			}
		case OpIn:
			if !present || !contains(s.Values, val) {
				return false
			}
		case OpNotIn:
			if present && contains(s.Values, val) {
				return false
			}
		}
	}
	return true
}

// tagSelected applies the tag-level predicates that do not require a manifest:
// exact tags, tag pattern, include filters and exclude filters.
func (q *Query) tagSelected(tag string) bool {
	v := parseSemVer(tag)

	if q.ExactTags != nil && !q.ExactTags[tag] {
		return false
	}
	if q.TagPattern != nil {
		if _, _, ok := q.TagPattern.Match(tag); !ok {
			return false
		}
	}
	for _, f := range q.Includes {
		if !f.matches(tag, v) {
			return false
		}
	}
	for _, f := range q.Excludes {
		if f.matches(tag, v) {
			return false
		}
	}
	return true
}

// sortAndLimit orders the artifacts per the query and truncates to Limit.
func (q *Query) sortAndLimit(arts []oci.Artifact) []oci.Artifact {
	key := q.Sort
	if key == "" {
		key = SortAlpha
	}
	less := func(i, j int) bool {
		switch key {
		case SortSemVer:
			return lessSemVer(arts[i], arts[j])
		case SortCreated:
			return lessCreated(arts[i], arts[j])
		default:
			return arts[i].Tag < arts[j].Tag
		}
	}
	sort.SliceStable(arts, less)
	if q.Order == OrderDesc {
		reverse(arts)
	}
	if q.Limit > 0 && len(arts) > q.Limit {
		arts = arts[:q.Limit]
	}
	return arts
}

func lessSemVer(a, b oci.Artifact) bool {
	switch {
	case a.SemVer != nil && b.SemVer != nil:
		return a.SemVer.LessThan(b.SemVer)
	case a.SemVer != nil:
		return false // valid semver sorts after non-semver
	case b.SemVer != nil:
		return true
	default:
		return a.Tag < b.Tag
	}
}

func lessCreated(a, b oci.Artifact) bool {
	switch {
	case a.CreatedAt != nil && b.CreatedAt != nil:
		return a.CreatedAt.Before(*b.CreatedAt)
	case a.CreatedAt != nil:
		return false
	case b.CreatedAt != nil:
		return true
	default:
		return a.Tag < b.Tag
	}
}

func reverse(a []oci.Artifact) {
	for i, j := 0, len(a)-1; i < j; i, j = i+1, j-1 {
		a[i], a[j] = a[j], a[i]
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// parseSemVer tolerantly parses a tag as semver, accepting an optional leading
// "v". Returns nil when the tag is not a valid version.
func parseSemVer(tag string) *semver.Version {
	v, err := semver.NewVersion(strings.TrimSpace(tag))
	if err != nil {
		return nil
	}
	return v
}
