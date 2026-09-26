package computeagent

import "testing"

func TestAgent_ShouldApplyACLRejectsStaleResourceVersion(t *testing.T) {
	a := &Agent{}

	if !a.shouldApplyACL("iface-1", 5) {
		t.Fatal("first command for an iface_id must always be applied")
	}
	a.recordAppliedACL("iface-1", 5)

	if a.shouldApplyACL("iface-1", 5) {
		t.Fatal("a redelivered command carrying the same resource_version must not re-apply")
	}
	if a.shouldApplyACL("iface-1", 3) {
		t.Fatal("an older resource_version than the last applied one must not re-apply")
	}
	if !a.shouldApplyACL("iface-1", 6) {
		t.Fatal("a newer resource_version must apply")
	}
	a.recordAppliedACL("iface-1", 6)
	if a.shouldApplyACL("iface-1", 6) {
		t.Fatal("resource_version 6 was just recorded as applied, must not re-apply")
	}

	// A different iface_id's history is independent.
	if !a.shouldApplyACL("iface-2", 1) {
		t.Fatal("a different iface_id must not be affected by iface-1's history")
	}
}
