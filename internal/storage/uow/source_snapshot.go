package uow

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// SourceSnapshotReaderSource exposes an atomic issue-plus-edge read on a UOW provider.
type SourceSnapshotReaderSource interface {
	SourceSnapshotReader() (issueops.SourceSnapshotReader, error)
}

type sourceSnapshotReader struct{ provider UnitOfWorkProvider }

func (p *doltSQLProvider) SourceSnapshotReader() (issueops.SourceSnapshotReader, error) {
	return NewSourceSnapshotReader(p)
}

func NewSourceSnapshotReader(provider UnitOfWorkProvider) (issueops.SourceSnapshotReader, error) {
	if isNilUnitOfWorkProvider(provider) {
		return nil, fmt.Errorf("new source snapshot reader: unit-of-work provider must not be nil")
	}
	return &sourceSnapshotReader{provider: provider}, nil
}

func (r *sourceSnapshotReader) ReadSourceSnapshot(ctx context.Context, id string) (issueops.SourceSnapshot, error) {
	return RunTxRead(ctx, r.provider, func(ctx context.Context, uw UnitOfWork) (issueops.SourceSnapshot, error) {
		row, _, err := operationIssue(ctx, uw, id, false)
		if err != nil {
			return issueops.SourceSnapshot{}, err
		}
		row, err = hydrateIssueOperation(ctx, uw, row, false, false)
		if err != nil {
			return issueops.SourceSnapshot{}, err
		}
		edges := row.Dependencies
		if edges == nil {
			edges = []*types.Dependency{}
		}
		row.Dependencies = nil
		return issueops.SourceSnapshot{Issue: row, OutgoingDependencies: edges}, nil
	})
}
