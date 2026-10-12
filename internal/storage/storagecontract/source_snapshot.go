package storagecontract

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

type SourceSnapshotFixture struct {
	IssuePrefix   string
	Reader        issueops.SourceSnapshotReader
	CreateIssue   func(context.Context, *types.Issue, string) error
	AddDependency func(context.Context, *types.Dependency, string) error
}

// RunSourceSnapshotReturnsTheIssueAndExactOutgoingEdges exercises the optional
// source-snapshot capability with a real stored edge. The result must contain
// the source row and edge record, rather than a hydrated target issue.
func RunSourceSnapshotReturnsTheIssueAndExactOutgoingEdges(t *testing.T, ctx context.Context, fixture SourceSnapshotFixture) {
	t.Helper()
	source := fixture.IssuePrefix + "-snapshot-source"
	target := fixture.IssuePrefix + "-snapshot-target"
	for _, id := range []string{source, target} {
		if err := fixture.CreateIssue(ctx, &types.Issue{
			ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
		}, "snapshot-seed"); err != nil {
			t.Fatalf("seed issue %s: %v", id, err)
		}
	}
	if err := fixture.AddDependency(ctx, &types.Dependency{
		IssueID: source, DependsOnID: target, Type: types.DepRelated,
	}, "snapshot-seed"); err != nil {
		t.Fatalf("seed dependency: %v", err)
	}
	snapshot, err := fixture.Reader.ReadSourceSnapshot(ctx, source)
	if err != nil {
		t.Fatalf("ReadSourceSnapshot: %v", err)
	}
	if snapshot.Issue == nil || snapshot.Issue.ID != source || snapshot.Issue.Title != source || snapshot.Issue.RowVersion == 0 {
		t.Fatalf("source row or revision missing: %+v", snapshot.Issue)
	}
	if snapshot.OutgoingDependencies == nil || len(snapshot.OutgoingDependencies) != 1 {
		t.Fatalf("outgoing edge set = %+v, want one stored edge", snapshot.OutgoingDependencies)
	}
	edge := snapshot.OutgoingDependencies[0]
	if edge.IssueID != source || edge.DependsOnID != target || edge.Type != types.DepRelated {
		t.Fatalf("stored edge = %+v, want %s -> %s (related)", edge, source, target)
	}
}
