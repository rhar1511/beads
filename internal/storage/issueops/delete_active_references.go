package issueops

import (
	"context"
	"fmt"

	"github.com/rhar1511/beads/internal/types"
	"github.com/rhar1511/beads/internal/workapi"
	publicops "github.com/rhar1511/beads/issueops"
)

// CheckActiveDeleteReferencesInTx checks surviving incoming owners in both
// planes. Only closed targets require closed owners; every surviving owner
// is returned with its plane for reference-cleanup fencing in this transaction.
// This read must follow the named-row version check and precede any mutation.
func CheckActiveDeleteReferencesInTx(ctx context.Context, tx DBTX, ids []string) (map[string]bool, error) {
	wisps, err := WispIDSetInTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	targets, err := GetIssuesByIDsInTx(ctx, tx, ids, wisps)
	if err != nil {
		return nil, err
	}
	deleting := make(map[string]bool, len(ids))
	for _, id := range ids {
		deleting[id] = true
	}
	closed := make(map[string]bool)
	for _, target := range targets {
		if target != nil && target.Status == types.StatusClosed {
			closed[target.ID] = true
		}
	}
	incoming, err := ExternalDependentsBySourceInTx(ctx, tx, ids, deleting)
	if err != nil {
		return nil, err
	}
	ownerIDs := make(map[string]bool)
	requireClosed := make(map[string]bool)
	for target, owners := range incoming {
		for _, id := range owners {
			ownerIDs[id] = true
			if closed[target] {
				requireClosed[id] = true
			}
		}
	}
	if len(ownerIDs) == 0 {
		return nil, nil
	}
	sortedIDs := workapi.SortedDeleteIDs(ownerIDs)
	ownerWisps, err := WispIDSetInTx(ctx, tx, sortedIDs)
	if err != nil {
		return nil, err
	}
	owners, err := GetIssuesByIDsInTx(ctx, tx, sortedIDs, ownerWisps)
	if err != nil {
		return nil, err
	}
	found := make(map[string]*types.Issue, len(owners))
	for _, owner := range owners {
		if owner != nil {
			found[owner.ID] = owner
		}
	}
	result := make(map[string]bool, len(sortedIDs))
	for _, id := range sortedIDs {
		owner := found[id]
		if owner == nil {
			return nil, fmt.Errorf("%w: reference owner %s is missing", publicops.ErrValidation, id)
		}
		if requireClosed[id] && owner.Status != types.StatusClosed {
			return nil, fmt.Errorf("%w: surviving reference owner %s is not closed", publicops.ErrActiveReferenceOwner, id)
		}
		_, isWisp := ownerWisps[id]
		result[id] = isWisp
	}
	return result, nil
}
