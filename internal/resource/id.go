package resource

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns a random identifier suitable for ObjectMeta.ID, e.g. "machine-3f9a1c2b4d5e6f70".
func NewID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}
