//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"

	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/storage/versioncontrolops"
	publicops "github.com/steveyegge/beads/issueops"
)

// Create's issue-row ownership survives SQL commit; all existing transaction
// ambiguity rules still apply. A history failure never replays its mutation.
func (s *EmbeddedDoltStore) runCreateIssueOperationTx(ctx context.Context, fn func(*sql.Tx) (publicops.CreateResult, storageissueops.CreateCommitScope, error)) (publicops.CreateResult, error) {
	var result publicops.CreateResult
	var scope storageissueops.CreateCommitScope
	if err := s.withConn(ctx, true, func(tx *sql.Tx) error {
		storageissueops.ResetProtectedMutationAttempt(ctx)
		var err error
		result, scope, err = fn(tx)
		return err
	}); err != nil {
		return result, err
	}
	if len(scope.Tables) == 0 {
		return result, nil
	}
	err := s.withMutatingDBConn(ctx, func(db versioncontrolops.DBConn) error {
		return versioncontrolops.StageCreateAndCommit(ctx, db, scope.Tables, scope.IssueRows, "bd: create issue", commitAuthor)
	})
	if err != nil {
		return result, wrapCommitIndeterminate("embeddeddolt: create stage and commit after SQL commit", err)
	}
	return result, nil
}
