package resource

import (
	"strings"
	"testing"
)

func TestValidateMetadata(t *testing.T) {
	ok := []Metadata{
		{},
		{Labels: map[string]string{"vpc.example.com/id": "vpc-123", "tier": "", "a": "b"}},
		{Annotations: map[string]string{"example.com/note": "free form: anything / goes \n here"}},
	}
	for _, m := range ok {
		if err := ValidateMetadata(m); err != nil {
			t.Errorf("ValidateMetadata(%+v) = %v, want nil", m, err)
		}
	}
	bad := []Metadata{
		{Labels: map[string]string{"": "x"}},
		{Labels: map[string]string{"-leading": "x"}},
		{Labels: map[string]string{"Upper.Case/name": "x"}},
		{Labels: map[string]string{"example.com/": "x"}},
		{Labels: map[string]string{strings.Repeat("a", 64): "x"}},
		{Labels: map[string]string{"k": "has space"}},
		{Labels: map[string]string{"k": strings.Repeat("v", 64)}},
		{Annotations: map[string]string{"bad key": "x"}},
		{Annotations: map[string]string{"k": strings.Repeat("v", maxAnnotationBytes)}},
	}
	for _, m := range bad {
		if err := ValidateMetadata(m); err == nil {
			t.Errorf("ValidateMetadata(%+v) = nil, want an error", m)
		}
	}
	tooMany := Metadata{Labels: map[string]string{}}
	for i := 0; i <= maxLabels; i++ {
		tooMany.Labels[strings.Repeat("k", 1)+string(rune('a'+i%26))+strings.Repeat("x", i/26)] = "v"
	}
	if err := ValidateMetadata(tooMany); err == nil {
		t.Errorf("ValidateMetadata with %d labels = nil, want an error", len(tooMany.Labels))
	}
}
