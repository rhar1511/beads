package uow

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
)

type sourceSnapshotIssueUseCase struct {
	domain.IssueUseCase
	issue *types.Issue
	err   error
	calls int
}

func (u *sourceSnapshotIssueUseCase) GetIssue(_ context.Context, id string) (*types.Issue, error) {
	u.calls++
	if u.issue != nil && u.issue.ID != id {
		return nil, storage.ErrNotFound
	}
	return u.issue, u.err
}

type sourceSnapshotDependencyUseCase struct {
	domain.DependencyUseCase
	edges map[string][]*types.Dependency
	err   error
	calls int
	ids   []string
}

func (u *sourceSnapshotDependencyUseCase) GetDurableIssueDependencyRecords(_ context.Context, ids []string) (map[string][]*types.Dependency, error) {
	u.calls++
	u.ids = append([]string(nil), ids...)
	return u.edges, u.err
}

type sourceSnapshotLabelUseCase struct {
	domain.LabelUseCase
	labels []string
	err    error
	calls  int
}

func (u *sourceSnapshotLabelUseCase) GetLabels(_ context.Context, _ string) ([]string, error) {
	u.calls++
	return append([]string(nil), u.labels...), u.err
}

func TestSourceSnapshotUsesOneUOWAndReturnsExplicitCompleteEdgeList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edges []*types.Dependency
	}{
		{name: "empty", edges: nil},
		{name: "nonempty", edges: []*types.Dependency{{IssueID: "bd-source", DependsOnID: "bd-target", Type: types.DepBlocks}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issueUC := &sourceSnapshotIssueUseCase{issue: &types.Issue{ID: "bd-source", RowVersion: -9223372036854775808}}
			labelsUC := &sourceSnapshotLabelUseCase{labels: []string{"q43", "review"}}
			depsUC := &sourceSnapshotDependencyUseCase{edges: map[string][]*types.Dependency{"bd-source": tc.edges}}
			uw := &mockUnitOfWork{issueUseCase: issueUC, labelUseCase: labelsUC, dependencyUseCase: depsUC}
			provider := &mockUnitOfWorkProvider{uows: []*mockUnitOfWork{uw}}
			reader, err := NewSourceSnapshotReader(provider)
			if err != nil {
				t.Fatalf("NewSourceSnapshotReader: %v", err)
			}
			snapshot, err := reader.ReadSourceSnapshot(context.Background(), "bd-source")
			if err != nil {
				t.Fatalf("ReadSourceSnapshot: %v", err)
			}
			if snapshot.Issue.RowVersion != -9223372036854775808 {
				t.Errorf("revision = %d, want signed-int64 minimum", snapshot.Issue.RowVersion)
			}
			if snapshot.OutgoingDependencies == nil {
				t.Fatal("successful edge read returned nil, want explicit empty or nonempty slice")
			}
			if len(snapshot.OutgoingDependencies) != len(tc.edges) {
				t.Fatalf("edges = %v, want %v", snapshot.OutgoingDependencies, tc.edges)
			}
			if len(snapshot.Issue.Labels) != 2 || snapshot.Issue.Labels[0] != "q43" || snapshot.Issue.Labels[1] != "review" {
				t.Errorf("labels = %v, want hydrated durable labels", snapshot.Issue.Labels)
			}
			if provider.newUOWCalls != 1 || !uw.closed || issueUC.calls != 1 || labelsUC.calls != 1 || depsUC.calls != 1 {
				t.Errorf("UOW/query counts provider=%d close=%t issue=%d labels=%d deps=%d, want one shared read transaction", provider.newUOWCalls, uw.closed, issueUC.calls, labelsUC.calls, depsUC.calls)
			}
			if len(depsUC.ids) != 1 || depsUC.ids[0] != "bd-source" {
				t.Errorf("dependency source IDs = %v, want [bd-source]", depsUC.ids)
			}
		})
	}
}

func TestSourceSnapshotPropagatesDependencyReadFailure(t *testing.T) {
	depErr := errors.New("dependency table unavailable")
	issueUC := &sourceSnapshotIssueUseCase{issue: &types.Issue{ID: "bd-source", RowVersion: 1}}
	labelsUC := &sourceSnapshotLabelUseCase{}
	depsUC := &sourceSnapshotDependencyUseCase{err: depErr}
	uw := &mockUnitOfWork{issueUseCase: issueUC, labelUseCase: labelsUC, dependencyUseCase: depsUC}
	provider := &mockUnitOfWorkProvider{uows: []*mockUnitOfWork{uw}}
	reader, err := NewSourceSnapshotReader(provider)
	if err != nil {
		t.Fatalf("NewSourceSnapshotReader: %v", err)
	}
	snapshot, err := reader.ReadSourceSnapshot(context.Background(), "bd-source")
	if snapshot != nil || !errors.Is(err, depErr) {
		t.Fatalf("ReadSourceSnapshot = (%v, %v), want no partial snapshot and dependency error", snapshot, err)
	}
	if provider.newUOWCalls != 1 || !uw.closed || issueUC.calls != 1 || labelsUC.calls != 1 || depsUC.calls != 1 {
		t.Errorf("UOW/query counts provider=%d close=%t issue=%d labels=%d deps=%d, want one shared read transaction", provider.newUOWCalls, uw.closed, issueUC.calls, labelsUC.calls, depsUC.calls)
	}
}

func TestSourceSnapshotFailsClosedForUnqualifiedRevision(t *testing.T) {
	issueUC := &sourceSnapshotIssueUseCase{issue: &types.Issue{ID: "bd-source"}}
	labelsUC := &sourceSnapshotLabelUseCase{}
	depsUC := &sourceSnapshotDependencyUseCase{}
	uw := &mockUnitOfWork{issueUseCase: issueUC, labelUseCase: labelsUC, dependencyUseCase: depsUC}
	provider := &mockUnitOfWorkProvider{uows: []*mockUnitOfWork{uw}}
	reader, err := NewSourceSnapshotReader(provider)
	if err != nil {
		t.Fatalf("NewSourceSnapshotReader: %v", err)
	}
	snapshot, err := reader.ReadSourceSnapshot(context.Background(), "bd-source")
	var unsupported *storage.ErrUnsupported
	if snapshot != nil || !errors.As(err, &unsupported) {
		t.Fatalf("ReadSourceSnapshot = (%v, %v), want unsupported without dependency read", snapshot, err)
	}
	if depsUC.calls != 0 {
		t.Errorf("dependency reads = %d, want none after a zero revision", depsUC.calls)
	}
	if labelsUC.calls != 0 {
		t.Errorf("label reads = %d, want none after a zero revision", labelsUC.calls)
	}
}
