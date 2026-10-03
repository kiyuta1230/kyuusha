package resource

import (
	"fmt"
	"regexp"
	"strings"
)

// Metadata is the caller-settable part of ObjectMeta a Create accepts
// alongside the spec: labels and annotations (see kyuusha.resource.v1.
// ObjectMeta's doc comment for what they are and aren't for).
type Metadata struct {
	Labels      map[string]string
	Annotations map[string]string
}

const (
	maxLabels          = 64
	maxAnnotationBytes = 64 << 10
)

var (
	labelNameRE   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)
	labelPrefixRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

// ValidateMetadata enforces the key/value rules ObjectMeta's doc comment
// documents. The returned error is a plain description; callers wrap it in
// their own ErrValidation.
func ValidateMetadata(m Metadata) error {
	if len(m.Labels) > maxLabels {
		return fmt.Errorf("labels: %d entries exceeds the limit of %d", len(m.Labels), maxLabels)
	}
	for k, v := range m.Labels {
		if err := validateKey(k); err != nil {
			return fmt.Errorf("labels: %w", err)
		}
		if v != "" && !labelNameRE.MatchString(v) {
			return fmt.Errorf("labels: value %q for key %q must be <=63 chars of [A-Za-z0-9._-], alphanumeric at both ends", v, k)
		}
	}
	total := 0
	for k, v := range m.Annotations {
		if err := validateKey(k); err != nil {
			return fmt.Errorf("annotations: %w", err)
		}
		total += len(k) + len(v)
	}
	if total > maxAnnotationBytes {
		return fmt.Errorf("annotations: %d bytes exceeds the limit of %d", total, maxAnnotationBytes)
	}
	return nil
}

func validateKey(k string) error {
	name := k
	if prefix, rest, ok := strings.Cut(k, "/"); ok {
		if len(prefix) > 253 || !labelPrefixRE.MatchString(prefix) {
			return fmt.Errorf("key %q: prefix must be a DNS subdomain (lowercase, <=253 chars)", k)
		}
		name = rest
	}
	if !labelNameRE.MatchString(name) {
		return fmt.Errorf("key %q: name must be 1-63 chars of [A-Za-z0-9._-], alphanumeric at both ends", k)
	}
	return nil
}
