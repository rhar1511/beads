package dolt

import (
	"context"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/storage/schema"
)

// ProtectedMutationReadiness verifies the schema required for protected
// mutation enforcement on this store's selected database.
func (s *DoltStore) ProtectedMutationReadiness(ctx context.Context) (schema.ProtectedMutationReadiness, error) {
	if s == nil || s.db == nil {
		return schema.ProtectedMutationReadiness{}, &storage.ErrUnsupported{Op: "ProtectedMutationReadiness", Backend: "nil"}
	}
	return schema.ProbeProtectedMutationReadiness(ctx, s.db)
}
