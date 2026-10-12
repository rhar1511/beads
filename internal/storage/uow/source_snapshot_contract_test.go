package uow

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/internal/storage/storagecontract"
)

func TestSourceSnapshotContract(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "snapshot")
	source, ok := provider.(SourceSnapshotReaderSource)
	if !ok {
		t.Fatalf("provider %T does not expose SourceSnapshotReader", provider)
	}
	reader, err := source.SourceSnapshotReader()
	if err != nil {
		t.Fatalf("SourceSnapshotReader: %v", err)
	}
	kit := newUOWRoleFixtureKit(provider, "snapshot")
	storagecontract.RunSourceSnapshotReturnsTheIssueAndExactOutgoingEdges(t, ctx, storagecontract.SourceSnapshotFixture{
		IssuePrefix: kit.IssuePrefix, Reader: reader,
		CreateIssue: kit.CreateIssue, AddDependency: kit.AddDependency,
	})
}
