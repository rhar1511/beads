package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

// ExecuteDurableSourceSnapshotInTx reads the canonical issue row and all of
// its outgoing durable dependency rows through the same transaction handle.
// The explicit table choice is important: GetIssueInTx falls back to wisps,
// whose row_lock is outside the durable Q43 aggregate revision domain.
func ExecuteDurableSourceSnapshotInTx(ctx context.Context, tx DBTX, id string) (*publicops.SourceSnapshot, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("source snapshot: %w: issue id is empty", storage.ErrValidation)
	}
	issue, err := getIssueFromTableInTx(ctx, tx, "issues", "labels", id)
	if err != nil {
		return nil, fmt.Errorf("source snapshot issue %s: %w", id, err)
	}
	if issue == nil {
		return nil, fmt.Errorf("source snapshot issue %s: %w", id, storage.ErrNotFound)
	}
	if issue.RowVersion == 0 {
		return nil, fmt.Errorf("source snapshot issue %s has no qualified aggregate revision: %w", id,
			&storage.ErrUnsupported{Op: "SourceSnapshot", Backend: "unqualified aggregate revision"})
	}
	edgesBySource, err := GetDependencyRecordsForIssuesFromTableInTx(ctx, tx, "dependencies", []string{id})
	if err != nil {
		return nil, fmt.Errorf("source snapshot outgoing dependencies for %s: %w", id, err)
	}
	edges := edgesBySource[id]
	if edges == nil {
		edges = make([]*types.Dependency, 0)
	}
	return &publicops.SourceSnapshot{Issue: issue, OutgoingDependencies: edges}, nil
}
