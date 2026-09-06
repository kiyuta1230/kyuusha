package resource

import (
	"context"
	"errors"
	"testing"
	"time"
)

// thing is a minimal resource type used only to exercise the generic Store
// directly, independent of any real service's domain model.
type thing struct {
	Meta ObjectMeta
	Name string // domain field, distinct from Meta.Name, just to prove obj round-trips
}

func (t *thing) GetID() string               { return t.Meta.ID }
func (t *thing) SetID(id string)             { t.Meta.ID = id }
func (t *thing) GetName() string             { return t.Meta.Name }
func (t *thing) SetName(name string)         { t.Meta.Name = name }
func (t *thing) GetTenantID() string         { return t.Meta.TenantID }
func (t *thing) SetTenantID(id string)       { t.Meta.TenantID = id }
func (t *thing) GetResourceVersion() int64   { return t.Meta.ResourceVersion }
func (t *thing) SetResourceVersion(rv int64) { t.Meta.ResourceVersion = rv }
func (t *thing) GetCreatedAt() time.Time     { return t.Meta.CreatedAt }
func (t *thing) SetCreatedAt(tm time.Time)   { t.Meta.CreatedAt = tm }
func (t *thing) GetDeletedAt() *time.Time    { return t.Meta.DeletedAt }
func (t *thing) SetDeletedAt(tm *time.Time)  { t.Meta.DeletedAt = tm }
func (t *thing) GetFinalizers() []Finalizer  { return t.Meta.Finalizers }
func (t *thing) SetFinalizers(f []Finalizer) { t.Meta.Finalizers = f }

var (
	errThingNotFound = errors.New("thing: not found")
	errThingConflict = errors.New("thing: conflict")
)

func newThingStore() *Store[thing, *thing] {
	return NewStore[thing, *thing]("thing", StoreErrors{
		NotFound: errThingNotFound,
		Conflict: errThingConflict,
	})
}

func TestStore_DeleteWithoutFinalizersRemovesImmediately(t *testing.T) {
	ctx := context.Background()
	s := newThingStore()

	out, err := s.Create(ctx, "tenant-a", "x", thing{Name: "unchanged"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Delete(ctx, "tenant-a", out.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "tenant-a", out.Meta.ID); err != errThingNotFound {
		t.Fatalf("Get after Delete: got %v, want errThingNotFound (immediate removal)", err)
	}
}

func TestStore_DeleteWithFinalizersMarksAndWaits(t *testing.T) {
	ctx := context.Background()
	s := newThingStore()

	out, err := s.Create(ctx, "tenant-a", "x", thing{Name: "unchanged"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	out.Meta.Finalizers = []Finalizer{{Name: "acme.corp/network-acl-cleanup"}}
	out, err = s.Update(ctx, out)
	if err != nil {
		t.Fatalf("Update to add finalizer: %v", err)
	}

	events, err := s.Watch(ctx, "tenant-a", out.Meta.ResourceVersion)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if err := s.Delete(ctx, "tenant-a", out.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The object must still exist, with DeletedAt set, rather than being
	// removed -- this is the whole point of a finalizer.
	got, err := s.Get(ctx, "tenant-a", out.Meta.ID)
	if err != nil {
		t.Fatalf("Get after Delete (finalizer pending): %v", err)
	}
	if got.Meta.DeletedAt == nil {
		t.Fatal("DeletedAt was not set by Delete")
	}
	if len(got.Meta.Finalizers) != 1 {
		t.Fatalf("Finalizers = %v, want the one still-present entry", got.Meta.Finalizers)
	}
	if got.Name != "unchanged" {
		t.Fatalf("domain field was touched by the finalizer machinery: %q", got.Name)
	}

	select {
	case e := <-events:
		if e.Type != EventModified {
			t.Fatalf("event after Delete-with-finalizers = %s, want Modified (not Deleted)", e.Type)
		}
		if e.Object.Meta.DeletedAt == nil {
			t.Fatal("Modified event's object doesn't carry DeletedAt")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the Modified event")
	}

	// Calling Delete again while finalizers are still present must be a
	// harmless no-op (not an error, not a second DeletedAt).
	firstDeletedAt := *got.Meta.DeletedAt
	if err := s.Delete(ctx, "tenant-a", out.Meta.ID); err != nil {
		t.Fatalf("second Delete while finalizers pending: %v", err)
	}
	got, err = s.Get(ctx, "tenant-a", out.Meta.ID)
	if err != nil {
		t.Fatalf("Get after second Delete: %v", err)
	}
	if !got.Meta.DeletedAt.Equal(firstDeletedAt) {
		t.Fatalf("DeletedAt changed on a second Delete call: %v vs %v", got.Meta.DeletedAt, firstDeletedAt)
	}

	// Clearing the last finalizer via Update must now actually remove it.
	got.Meta.Finalizers = nil
	if _, err := s.Update(ctx, got); err != nil {
		t.Fatalf("Update clearing the last finalizer: %v", err)
	}
	if _, err := s.Get(ctx, "tenant-a", out.Meta.ID); err != errThingNotFound {
		t.Fatalf("Get after clearing the last finalizer: got %v, want errThingNotFound", err)
	}

	select {
	case e := <-events:
		if e.Type != EventDeleted {
			t.Fatalf("event after clearing the last finalizer = %s, want Deleted", e.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the Deleted event")
	}
}

func TestStore_UpdateCannotResurrectAPendingDeletion(t *testing.T) {
	ctx := context.Background()
	s := newThingStore()

	out, err := s.Create(ctx, "tenant-a", "x", thing{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	out.Meta.Finalizers = []Finalizer{{Name: "acme.corp/cleanup"}}
	out, err = s.Update(ctx, out)
	if err != nil {
		t.Fatalf("Update to add finalizer: %v", err)
	}
	if err := s.Delete(ctx, "tenant-a", out.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	pending, err := s.Get(ctx, "tenant-a", out.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// A client that tries to clear DeletedAt (while still holding the
	// finalizer) must not succeed in "undeleting" the object: the server
	// keeps its own DeletedAt regardless of what the client sent.
	pending.Meta.DeletedAt = nil
	updated, err := s.Update(ctx, pending)
	if err != nil {
		t.Fatalf("Update attempting to clear DeletedAt: %v", err)
	}
	if updated.Meta.DeletedAt == nil {
		t.Fatal("DeletedAt was cleared by a client-supplied Update -- deletion must be one-way")
	}
}
