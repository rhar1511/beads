package uow

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage/domain"
	storageops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

func TestUOWProtectedDeleteChecksCascadeDescendantsAndLowLevelDeletes(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "pdel")
	kit := newUOWRoleFixtureKit(provider, "pdel")

	root := &types.Issue{ID: "pdel-root", Title: "root", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	descendant := &types.Issue{ID: "pdel-descendant", Title: "descendant", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	for _, issue := range []*types.Issue{root, descendant} {
		if err := kit.CreateIssue(ctx, issue, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", issue.ID, err)
		}
	}
	if err := kit.AddDependency(ctx, &types.Dependency{IssueID: descendant.ID, DependsOnID: root.ID, Type: types.DepParentChild}, "seed"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	classUOWIssue(t, ctx, provider, descendant.ID)

	deleterSource, ok := provider.(DeleterSource)
	if !ok {
		t.Fatalf("provider %T does not offer Deleter", provider)
	}
	deleter, err := deleterSource.Deleter()
	if err != nil {
		t.Fatalf("Deleter: %v", err)
	}
	for _, req := range []publicops.DeleteRequest{
		{IDs: []string{root.ID}, Cascade: true},
		{IDs: []string{root.ID}, Cascade: true, DryRun: true},
	} {
		if _, err := deleter.Delete(ctx, req); !errors.Is(err, publicops.ErrProtectedRecord) {
			t.Fatalf("Delete(%+v) error = %v, want ErrProtectedRecord", req, err)
		}
	}

	// The use-case seam is also guarded when a caller bypasses the public role.
	err = RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
		_, err := uw.IssueUseCase().DeleteIssues(ctx, domain.DeleteIssuesParams{
			IDs:     []string{descendant.ID},
			Cascade: true,
		}, "low-level")
		return "", err
	})
	if !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("direct IssueUseCase.DeleteIssues error = %v, want ErrProtectedRecord", err)
	}

	for _, id := range []string{root.ID, descendant.ID} {
		var count int
		if err := kit.QueryScalar(ctx, "SELECT COUNT(*) FROM issues WHERE id = ?", []any{id}, &count); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if count != 1 {
			t.Errorf("issue %s count after refused delete = %d, want 1", id, count)
		}
	}
}

func TestUOWProtectedSweepRefusesCandidatesAndDryRun(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "psweep")
	kit := newUOWRoleFixtureKit(provider, "psweep")

	candidate := &types.Issue{ID: "psweep-candidate", Title: "closed candidate", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := kit.CreateIssue(ctx, candidate, "seed"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
		if _, err := uw.RawSQLUseCase().Exec(ctx, "UPDATE issues SET status = 'closed', closed_at = '2020-01-01 00:00:00' WHERE id = ?", candidate.ID); err != nil {
			return "", err
		}
		if _, err := uw.RawSQLUseCase().Exec(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", candidate.ID, "source"); err != nil {
			return "", err
		}
		return "seed protected sweep candidate", nil
	}); err != nil {
		t.Fatalf("seed protected candidate: %v", err)
	}

	sweeperSource, ok := provider.(SweeperSource)
	if !ok {
		t.Fatalf("provider %T does not offer Sweeper", provider)
	}
	sweeper, err := sweeperSource.Sweeper()
	if err != nil {
		t.Fatalf("Sweeper: %v", err)
	}
	for _, req := range []publicops.SweepRequest{
		{Tier: publicops.SweepDurable, IDPattern: candidate.ID},
		{Tier: publicops.SweepDurable, IDPattern: candidate.ID, DryRun: true},
	} {
		if _, err := sweeper.Sweep(ctx, req); !errors.Is(err, publicops.ErrProtectedRecord) {
			t.Fatalf("Sweep(%+v) error = %v, want ErrProtectedRecord", req, err)
		}
	}

	var count int
	if err := kit.QueryScalar(ctx, "SELECT COUNT(*) FROM issues WHERE id = ?", []any{candidate.ID}, &count); err != nil {
		t.Fatalf("count candidate: %v", err)
	}
	if count != 1 {
		t.Fatalf("candidate count after refused sweep = %d, want 1", count)
	}
}

func classUOWIssue(t *testing.T, ctx context.Context, provider UnitOfWorkProvider, id string) {
	t.Helper()
	if err := RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
		_, err := uw.RawSQLUseCase().Exec(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", id, "source")
		return "classify protected issue", err
	}); err != nil {
		t.Fatalf("classify %s: %v", id, err)
	}
}

func TestUOWDeletePermitBindsActiveReferenceConstraint(t *testing.T) {
	ctx := context.Background()
	provider := newUOWRoleFixtureProvider(t, ctx, "pdigest")
	kit := newUOWRoleFixtureKit(provider, "pdigest")
	id := "pdigest-target"
	if err := kit.CreateIssue(ctx, &types.Issue{
		ID: id, Title: "protected target", Status: types.StatusOpen,
		Priority: 2, IssueType: types.TypeTask,
	}, "seed"); err != nil {
		t.Fatal(err)
	}
	classUOWIssue(t, ctx, provider, id)
	deleter, err := NewDeleter(provider)
	if err != nil {
		t.Fatal(err)
	}
	request := publicops.DeleteRequest{IDs: []string{id}, Force: true}
	digest, err := publicops.ProtectedMutationDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	request.ProtectedPermit = "bound-delete"
	verifier := protectedBatchVerifierFunc(func(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		if permit != request.ProtectedPermit || scope.Operation != "issue.delete" || !equalProtectedBatchIDs(scope.ResourceIDs, []string{id}) || scope.Digest != digest {
			return publicops.ProtectedMutationVerification{}, publicops.ErrProtectedRecord
		}
		return publicops.ProtectedMutationVerification{ReplayID: "bound-delete-once"}, nil
	})
	protectedCtx := storageops.WithProtectedMutationVerifier(ctx, verifier, "test-audience", "test-project", "test-database")
	changed := request
	changed.ProtectActiveReferences = true
	if _, err := deleter.Delete(protectedCtx, changed); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("changed constraint reused original permit: %v", err)
	}
	var rows int
	if err := kit.QueryScalar(ctx, "SELECT COUNT(*) FROM protected_issue_records WHERE issue_id=?", []any{id}, &rows); err != nil || rows != 1 {
		t.Fatalf("rejected constraint removed classification: count=%d err=%v", rows, err)
	}
	if _, err := deleter.Delete(protectedCtx, request); err != nil {
		t.Fatalf("rejected constraint consumed original permit: %v", err)
	}
}
