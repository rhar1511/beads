package dolt

import (
	"context"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/storage/schema"
)

// BatchApplyReceiptReadiness verifies the migration and table required before
// the optional durable apply-batch receipt is advertised over HTTP.
func (s *DoltStore) BatchApplyReceiptReadiness(ctx context.Context) (schema.BatchApplyReceiptReadiness, error) {
	if s == nil || s.db == nil {
		return schema.BatchApplyReceiptReadiness{}, &storage.ErrUnsupported{Op: "BatchApplyReceiptReadiness", Backend: "nil"}
	}
	return schema.ProbeBatchApplyReceiptReadiness(ctx, s.db)
}
