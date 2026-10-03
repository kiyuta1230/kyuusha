package resource

import (
	"context"
	"errors"
	"fmt"

	"github.com/kiyuta1230/kyuusha/internal/authn"
)

// ErrFinalizerNotOwner is CheckFinalizerMutation's rejection.
var ErrFinalizerNotOwner = errors.New("finalizer not owned by caller")

// CheckFinalizerMutation reconciles the Finalizers an Update caller
// requested against what's actually stored, the same fetch-then-merge
// shape as every other server-stamped field: a client can never set or
// change an entry's AddedBy. A newly added entry (present in requested,
// absent from stored) gets AddedBy stamped from the caller's real,
// propagated identity (internal/authn/propagate.go) -- any client-supplied
// AddedBy is discarded. Removing an entry (present in stored, absent from
// requested) requires the caller's sub to match that entry's AddedBy, or
// the admin role, UNLESS the stored AddedBy is itself empty (an entry added
// before this identity plumbing existed, or by a caller that never had a
// propagated identity -- e.g. an internal caller that bypasses api-gateway):
// an unowned entry stays removable by anyone, exactly like before this
// check existed. Any other mismatch is rejected with ErrFinalizerNotOwner
// (callers wrap it in their own ErrValidation) and the finalizer is left in
// place, unchanged. See docs/architecture.md
// "Finalizer" for why this authorization exists at all -- clearing someone
// else's finalizer would let a tenant remove a hold an external controller
// placed to protect a resource it doesn't own.
func CheckFinalizerMutation(ctx context.Context, stored, requested []Finalizer) ([]Finalizer, error) {
	storedByName := make(map[string]Finalizer, len(stored))
	for _, f := range stored {
		storedByName[f.Name] = f
	}
	requestedNames := make(map[string]struct{}, len(requested))

	callerSub, _ := authn.CallerSubFromContext(ctx)
	isAdmin := authn.CallerIsAdminFromContext(ctx)

	out := make([]Finalizer, len(requested))
	for i, f := range requested {
		requestedNames[f.Name] = struct{}{}
		if existing, ok := storedByName[f.Name]; ok {
			out[i] = existing
			continue
		}
		out[i] = Finalizer{Name: f.Name, AddedBy: callerSub}
	}

	for _, f := range stored {
		if _, stillPresent := requestedNames[f.Name]; stillPresent {
			continue
		}
		if f.AddedBy != "" && !isAdmin && callerSub != f.AddedBy {
			return nil, fmt.Errorf("%w: finalizer %q can only be removed by %q or an admin", ErrFinalizerNotOwner, f.Name, f.AddedBy)
		}
	}

	return out, nil
}
