package dolt

import (
	"context"
	"database/sql"

	"github.com/rhar1511/beads/internal/storage/issueops"
	"github.com/rhar1511/beads/internal/types"
)

// UpdateIssueID updates an issue ID and all its references.
func (s *DoltStore) UpdateIssueID(ctx context.Context, oldID, newID string, issue *types.Issue, actor string) error {
	return s.withRetryTx(ctx, func(tx *sql.Tx) error {
		return issueops.UpdateIssueIDInTx(ctx, tx, oldID, newID, issue, actor)
	})
}
