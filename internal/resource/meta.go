// Package resource holds the types shared by every top-level kyuusha
// resource (Machine, Subnet, Volume, ...), mirroring docs/architecture.md's
// "リソース共通の型" section.
package resource

import "time"

// ObjectMeta is embedded in every top-level resource.
type ObjectMeta struct {
	ID              string
	Name            string
	TenantID        string
	ResourceVersion int64
	CreatedAt       time.Time
	DeletedAt       *time.Time // nil unless soft-deleted
}
