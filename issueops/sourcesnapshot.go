// Package issueops declares Beads' stable, storage-neutral operation roles.
package issueops

import (
	"context"

	"github.com/rhar1511/beads/internal/types"
)

// SourceSnapshot is one durable issue row and every outgoing durable
// dependency row observed in the same database transaction. RowVersion is
// the issue row's opaque aggregate revision; callers must compare it only for
// equality. A successful read always returns a non-nil dependency slice, with
// an empty slice meaning the read completed and found no outgoing edges.
type SourceSnapshot struct {
	Issue                *types.Issue
	OutgoingDependencies []*types.Dependency
}

// SourceSnapshotReader reads the exact durable source row together with its
// complete outgoing dependency list from one database snapshot. It does not
// resolve wisps or hydrate dependency targets.
type SourceSnapshotReader interface {
	ReadSourceSnapshot(context.Context, string) (*SourceSnapshot, error)
}
