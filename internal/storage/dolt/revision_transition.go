package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/storage"
	storeops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/issueops"
)

// RevisionTransition returns the issue revision-fenced transition role.
func (s *DoltStore) RevisionTransition() (issueops.RevisionTransition, error) {
	if s == nil {
		return nil, &storage.ErrUnsupported{Op: "RevisionTransition", Backend: "nil"}
	}
	return &revisionTransition{store: s}, nil
}

type revisionTransition struct{ store *DoltStore }

var _ issueops.RevisionTransition = (*revisionTransition)(nil)

func (r *revisionTransition) TransitionMetadata(ctx context.Context, req issueops.RevisionTransitionRequest) (issueops.RevisionTransitionResult, error) {
	plan, err := storage.PlanRevisionTransition(req)
	if err != nil {
		return issueops.RevisionTransitionResult{}, err
	}
	var preflight issueops.RevisionTransitionResult
	var preflightDone bool
	if err := r.store.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		preflight, preflightDone, err = storeops.PreflightRevisionTransitionInTx(ctx, tx, plan)
		return err
	}); err != nil {
		return issueops.RevisionTransitionResult{}, err
	}
	if preflightDone {
		return preflight, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		var result issueops.RevisionTransitionResult
		err := r.store.withRetryTx(ctx, func(tx *sql.Tx) error {
			transition, write, err := storeops.TransitionMetadataInTx(ctx, tx, plan)
			if err != nil {
				return err
			}
			result = transition
			if !write.Wrote {
				return nil
			}
			return r.store.doltAddAndCommitInTx(ctx, tx,
				[]string{"issues", "events", "issue_transition_receipts"},
				fmt.Sprintf("bd: revision transition %s", plan.ReceiptID))
		})
		if errors.Is(err, storeops.ErrRevisionTransitionReceiptRace) {
			continue
		}
		if err != nil {
			return issueops.RevisionTransitionResult{}, err
		}
		return result, nil
	}
	return issueops.RevisionTransitionResult{}, fmt.Errorf("revision transition receipt %s did not converge after concurrent receipt creation: %w", plan.ReceiptID, issueops.ErrTransitionReceiptConflict)
}

func (r *revisionTransition) GetReceipt(ctx context.Context, receiptID string) (*issueops.RevisionTransitionReceipt, error) {
	if err := storage.ValidateRevisionTransitionReceiptID(receiptID); err != nil {
		return nil, err
	}
	var receipt *issueops.RevisionTransitionReceipt
	if err := r.store.withRetryTx(ctx, func(tx *sql.Tx) error {
		var err error
		receipt, err = storeops.GetRevisionTransitionReceiptInTx(ctx, tx, receiptID)
		return err
	}); err != nil {
		return nil, err
	}
	return receipt, nil
}
