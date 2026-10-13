//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// BatchApplyReceiptReadiness verifies the receipt table on the selected
// embedded database using a read-only connection.
func (s *EmbeddedDoltStore) BatchApplyReceiptReadiness(ctx context.Context) (schema.BatchApplyReceiptReadiness, error) {
	if s == nil {
		return schema.BatchApplyReceiptReadiness{}, &storage.ErrUnsupported{Op: "BatchApplyReceiptReadiness", Backend: "nil"}
	}
	var readiness schema.BatchApplyReceiptReadiness
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		readiness, err = schema.ProbeBatchApplyReceiptReadiness(ctx, tx)
		return err
	})
	return readiness, err
}
