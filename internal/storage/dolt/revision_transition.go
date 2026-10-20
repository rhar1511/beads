package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rhar1511/beads/internal/storage"
	storeops "github.com/rhar1511/beads/internal/storage/issueops"
	"github.com/rhar1511/beads/internal/storage/schema"
	"github.com/rhar1511/beads/issueops"
)

// RevisionTransition returns the issue revision-fenced transition role.
func (s *DoltStore) RevisionTransition() (issueops.RevisionTransition, error) {
	if s == nil {
		return nil, &storage.ErrUnsupported{Op: "RevisionTransition", Backend: "nil"}
	}
	return &revisionTransition{store: s}, nil
}

// RevisionTransitionReadiness verifies the migration and backfill state that
// must hold before this store's transition role is advertised over HTTP.
func (s *DoltStore) RevisionTransitionReadiness(ctx context.Context) (schema.RevisionTransitionReadiness, error) {
	if s == nil || s.db == nil {
		return schema.RevisionTransitionReadiness{}, &storage.ErrUnsupported{Op: "RevisionTransitionReadiness", Backend: "nil"}
	}
	return schema.ProbeRevisionTransitionReadiness(ctx, s.db)
}

type revisionTransition struct{ store *DoltStore }

var _ issueops.RevisionTransition = (*revisionTransition)(nil)
var _ issueops.RevisionTransitionPatcher = (*revisionTransition)(nil)

func (r *revisionTransition) TransitionMetadata(ctx context.Context, req issueops.RevisionTransitionRequest) (issueops.RevisionTransitionResult, error) {
	plan, err := storage.PlanRevisionTransition(req)
	if err != nil {
		return issueops.RevisionTransitionResult{}, err
	}
	digest, err := issueops.ProtectedMutationDigest(req)
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
			if err := storeops.RequireProtectedMutation(ctx, tx, req.ProtectedPermit, "issue.revision_transition", []string{plan.IssueID}, digest, ""); err != nil {
				return err
			}
			transition, write, err := storeops.TransitionMetadataInTx(ctx, tx, plan)
			if err != nil {
				return err
			}
			result = transition
			if !write.Wrote {
				if storeops.ProtectedMutationPermitUseWritten(ctx) {
					return r.store.doltAddAndCommitInTx(ctx, tx, []string{"protected_mutation_permit_uses"}, "bd: protected mutation permit use")
				}
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
	if err := r.store.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		receipt, err = storeops.GetRevisionTransitionReceiptInTx(ctx, tx, receiptID)
		return err
	}); err != nil {
		return nil, err
	}
	return receipt, nil
}

func (r *revisionTransition) TransitionPatch(ctx context.Context, req issueops.RevisionTransitionPatchRequest) (issueops.RevisionTransitionPatchResult, error) {
	plan, err := storage.PlanRevisionTransitionPatch(req)
	if err != nil {
		return issueops.RevisionTransitionPatchResult{}, err
	}
	digest, err := issueops.ProtectedMutationDigest(plan)
	if err != nil {
		return issueops.RevisionTransitionPatchResult{}, err
	}
	var preflight issueops.RevisionTransitionPatchResult
	var preflightDone bool
	if err := r.store.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		preflight, preflightDone, err = storeops.PreflightRevisionTransitionPatchInTx(ctx, tx, plan)
		return err
	}); err != nil {
		return issueops.RevisionTransitionPatchResult{}, err
	}
	if preflightDone {
		return preflight, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		var result issueops.RevisionTransitionPatchResult
		err := r.store.withRetryTx(ctx, func(tx *sql.Tx) error {
			if err := storeops.RequireAuthorizedMutation(ctx, tx, req.ProtectedPermit, "issue.revision_transition_patch", []string{plan.IssueID}, digest); err != nil {
				return err
			}
			transition, write, err := storeops.TransitionPatchInTx(ctx, tx, plan)
			if err != nil {
				return err
			}
			result = transition
			if !write.Wrote {
				if storeops.ProtectedMutationPermitUseWritten(ctx) {
					return r.store.doltAddAndCommitInTx(ctx, tx, []string{"protected_mutation_permit_uses"}, "bd: protected patch receipt replay")
				}
				return nil
			}
			tables := []string{"issues", "events", "issue_transition_receipts"}
			if storeops.ProtectedMutationPermitUseWritten(ctx) {
				tables = append(tables, "protected_mutation_permit_uses")
			}
			return r.store.doltAddAndCommitInTx(ctx, tx, tables, fmt.Sprintf("bd: revision transition patch %s", plan.ReceiptID))
		})
		if errors.Is(err, storeops.ErrRevisionTransitionReceiptRace) {
			continue
		}
		if err != nil {
			return issueops.RevisionTransitionPatchResult{}, err
		}
		return result, nil
	}
	return issueops.RevisionTransitionPatchResult{}, fmt.Errorf("revision transition patch receipt %s did not converge after concurrent receipt creation: %w", plan.ReceiptID, issueops.ErrTransitionReceiptConflict)
}

func (r *revisionTransition) GetPatchReceipt(ctx context.Context, receiptID string) (*issueops.RevisionTransitionPatchReceipt, error) {
	if err := storage.ValidateRevisionTransitionReceiptID(receiptID); err != nil {
		return nil, err
	}
	var receipt *issueops.RevisionTransitionPatchReceipt
	if err := r.store.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		receipt, err = storeops.GetRevisionTransitionPatchReceiptInTx(ctx, tx, receiptID)
		return err
	}); err != nil {
		return nil, err
	}
	return receipt, nil
}
