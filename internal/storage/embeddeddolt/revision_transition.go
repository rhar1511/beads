//go:build cgo

package embeddeddolt

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
func (s *EmbeddedDoltStore) RevisionTransition() (issueops.RevisionTransition, error) {
	if s == nil {
		return nil, &storage.ErrUnsupported{Op: "RevisionTransition", Backend: "nil"}
	}
	return &revisionTransition{store: s}, nil
}

// RevisionTransitionReadiness verifies the migration and backfill state that
// must hold before this store's transition role is advertised over HTTP.
func (s *EmbeddedDoltStore) RevisionTransitionReadiness(ctx context.Context) (schema.RevisionTransitionReadiness, error) {
	if s == nil {
		return schema.RevisionTransitionReadiness{}, &storage.ErrUnsupported{Op: "RevisionTransitionReadiness", Backend: "nil"}
	}
	var readiness schema.RevisionTransitionReadiness
	err := s.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		readiness, err = schema.ProbeRevisionTransitionReadiness(ctx, tx)
		return err
	})
	return readiness, err
}

type revisionTransition struct{ store *EmbeddedDoltStore }

var _ issueops.RevisionTransition = (*revisionTransition)(nil)

func (r *revisionTransition) TransitionMetadata(ctx context.Context, req issueops.RevisionTransitionRequest) (issueops.RevisionTransitionResult, error) {
	plan, err := storage.PlanRevisionTransition(req)
	if err != nil {
		return issueops.RevisionTransitionResult{}, err
	}
	var preflight issueops.RevisionTransitionResult
	var preflightDone bool
	if err := r.store.withConn(ctx, false, func(tx *sql.Tx) error {
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
		err := r.store.runIssueOperationTxWithMessage(ctx, func(tx *sql.Tx) (storeops.ChangedTables, string, error) {
			transition, write, err := storeops.TransitionMetadataInTx(ctx, tx, plan)
			if err != nil {
				return nil, "", err
			}
			result = transition
			if !write.Wrote {
				return nil, "", nil
			}
			return write.Tables, fmt.Sprintf("bd: revision transition %s", plan.ReceiptID), nil
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
	if err := r.store.withConn(ctx, false, func(tx *sql.Tx) error {
		var err error
		receipt, err = storeops.GetRevisionTransitionReceiptInTx(ctx, tx, receiptID)
		return err
	}); err != nil {
		return nil, err
	}
	return receipt, nil
}
