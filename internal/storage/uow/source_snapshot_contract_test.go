package uow

import (
	"context"
	"testing"

	"github.com/steveyegge/beads/backend/conformance"
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
	conformance.RunSourceSnapshotReturnsTheIssueAndExactOutgoingEdges(t, ctx, conformance.SourceSnapshotFixture{
		IssuePrefix: kit.IssuePrefix, Reader: reader,
		CreateIssue: kit.CreateIssue, AddDependency: kit.AddDependency,
	})
}
