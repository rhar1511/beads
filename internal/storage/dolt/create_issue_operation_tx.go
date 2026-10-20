package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	storageissueops "github.com/rhar1511/beads/internal/storage/issueops"
	"github.com/rhar1511/beads/internal/storage/versioncontrolops"
	publicops "github.com/rhar1511/beads/issueops"
)

// Create resets its captured scope on every SQL attempt. An indeterminate
// data commit returns before any staging. After a known data commit, missing
// history keeps runIssueOperationTx's existing logged/deferred disposition;
// returning a mutation retry signal would double-apply that persisted create.
func (s *DoltStore) runCreateIssueOperationTx(ctx context.Context, fn func(*sql.Tx) (publicops.CreateResult, storageissueops.CreateCommitScope, error)) (publicops.CreateResult, error) {
	var result publicops.CreateResult
	var scope storageissueops.CreateCommitScope
	if err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		result = publicops.CreateResult{}
		scope = storageissueops.CreateCommitScope{}
		var err error
		result, scope, err = fn(tx)
		return err
	}); err != nil {
		return result, err
	}
	if len(scope.Tables) == 0 {
		return result, nil
	}
	if err := s.createCommitPostTx(ctx, scope); err != nil {
		doltMetrics.postTxCommitDropped.Add(ctx, 1)
		log.Printf("dolt: post-tx create commit failed (data already committed; history deferred, do not replay mutation): %v", err)
	}
	return result, nil
}

func (s *DoltStore) createCommitPostTx(ctx context.Context, scope storageissueops.CreateCommitScope) error {
	ctx = context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, postTxCommitMaxElapsed+postTxCommitGrace)
	defer cancel()
	// Row staging can make partial progress. Unlike table-only publication,
	// this path makes one bounded attempt and retains failures for diagnosis.
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return s.recordDoltPublicationFailure(ctx, fmt.Errorf("acquire create publication connection: %w: %w", err, ErrCommitIndeterminate))
		}
		defer conn.Close()
		if err := versioncontrolops.StageCreateAndCommit(ctx, conn, scope.Tables, scope.IssueRows, "bd: create issue", s.commitAuthorString()); err != nil {
			return s.recordDoltPublicationFailure(ctx, fmt.Errorf("create publication after SQL commit: %w: %w", err, ErrCommitIndeterminate))
		}
		return nil
	})
}
