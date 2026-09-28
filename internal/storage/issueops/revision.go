package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
)

// BumpIssueRevisionInTx remints one issue's opaque row_lock while preserving
// updated_at. Call it in the same transaction as a source-graph or label
// mutation whose data is not stored on the issue row itself.
func BumpIssueRevisionInTx(ctx context.Context, tx DBTX, issueID string, isWisp bool) error {
	issueTable, _, _, _ := WispTableRouting(isWisp)
	var current sql.NullInt64
	err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT row_lock FROM %s WHERE id = ?", issueTable), issueID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: issue %s", storage.ErrNotFound, issueID)
	}
	if err != nil {
		return fmt.Errorf("read issue revision for %s: %w", issueID, err)
	}
	previous := int64(0)
	if current.Valid {
		previous = current.Int64
	}
	next := freshRowLockDifferentThan(previous)
	res, err := tx.ExecContext(ctx, fmt.Sprintf(
		"UPDATE %s SET row_lock = ?, updated_at = updated_at WHERE id = ?", issueTable), next, issueID)
	if err != nil {
		return fmt.Errorf("bump issue revision for %s: %w", issueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("bump issue revision for %s: rows affected: %w", issueID, err)
	}
	if n != 1 {
		return fmt.Errorf("bump issue revision for %s: changed %d rows", issueID, n)
	}
	return nil
}

func freshRowLockDifferentThan(previous int64) int64 {
	next := freshRowLock()
	for next == 0 || next == previous {
		next = freshRowLock()
	}
	return next
}
