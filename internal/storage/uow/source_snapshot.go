package uow

import (
	"context"
	"fmt"

	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/types"
	publicops "github.com/rhar1511/beads/issueops"
)

// SourceSnapshotReaderSource is the accessor for the atomic durable source
// read. It parallels the store's optional SourceSnapshotReader accessor.
type SourceSnapshotReaderSource interface {
	SourceSnapshotReader() (publicops.SourceSnapshotReader, error)
}

type sourceSnapshotReader struct {
	provider UnitOfWorkProvider
}

func (p *doltSQLProvider) SourceSnapshotReader() (publicops.SourceSnapshotReader, error) {
	return NewSourceSnapshotReader(p)
}

// NewSourceSnapshotReader constructs a source snapshot role backed by one
// unit of work per read.
func NewSourceSnapshotReader(provider UnitOfWorkProvider) (publicops.SourceSnapshotReader, error) {
	if isNilUnitOfWorkProvider(provider) {
		return nil, fmt.Errorf("new source snapshot reader: unit-of-work provider must not be nil")
	}
	return &sourceSnapshotReader{provider: provider}, nil
}

var _ publicops.SourceSnapshotReader = (*sourceSnapshotReader)(nil)

func (r *sourceSnapshotReader) ReadSourceSnapshot(ctx context.Context, id string) (*publicops.SourceSnapshot, error) {
	return RunTxRead(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (*publicops.SourceSnapshot, error) {
		issue, err := uw.IssueUseCase().GetIssue(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("source snapshot issue %s: %w", id, err)
		}
		if issue == nil || issue.ID != id {
			return nil, fmt.Errorf("source snapshot issue %s: %w", id, storage.ErrNotFound)
		}
		if issue.RowVersion == 0 {
			return nil, fmt.Errorf("source snapshot issue %s has no qualified aggregate revision: %w", id,
				&storage.ErrUnsupported{Op: "SourceSnapshot", Backend: "unqualified aggregate revision"})
		}
		issue.Labels, err = uw.LabelUseCase().GetLabels(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("source snapshot labels for %s: %w", id, err)
		}
		bySource, err := uw.DependencyUseCase().GetDurableIssueDependencyRecords(ctx, []string{id})
		if err != nil {
			return nil, fmt.Errorf("source snapshot outgoing dependencies for %s: %w", id, err)
		}
		dependencies := bySource[id]
		if dependencies == nil {
			dependencies = make([]*types.Dependency, 0)
		}
		return &publicops.SourceSnapshot{Issue: issue, OutgoingDependencies: dependencies}, nil
	})
}
