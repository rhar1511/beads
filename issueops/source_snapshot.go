package issueops

import (
	"context"

	"github.com/steveyegge/beads/internal/types"
)

// SourceSnapshot is one issue and its stored outgoing edges read in one
// transaction. Expanded dependency issues are deliberately not substituted for
// the edge records: an edge can outlive its target row.
type SourceSnapshot struct {
	Issue                *types.Issue        `json:"-"`
	OutgoingDependencies []*types.Dependency `json:"outgoing_dependencies"`
}

// SourceSnapshotReader reads the source row and its graph evidence atomically.
type SourceSnapshotReader interface {
	ReadSourceSnapshot(context.Context, string) (SourceSnapshot, error)
}
