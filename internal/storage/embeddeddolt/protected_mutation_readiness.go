//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/storage/schema"
)

// ProtectedMutationReadiness verifies the schema required for protected
// mutation enforcement on the selected embedded database using a read-only
// transaction.
func (s *EmbeddedDoltStore) ProtectedMutationReadiness(ctx context.Context) (schema.ProtectedMutationReadiness, error) {
	if s == nil {
		return schema.ProtectedMutationReadiness{}, &storage.ErrUnsupported{Op: "ProtectedMutationReadiness", Backend: "nil"}
	}
	var readiness schema.ProtectedMutationReadiness
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		readiness, err = schema.ProbeProtectedMutationReadiness(ctx, tx)
		return err
	})
	return readiness, err
}
