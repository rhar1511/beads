package dolt

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

type protectedMutationVerifierFunc func(context.Context, publicops.ProtectedMutationPermit, publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error)

func (f protectedMutationVerifierFunc) Verify(ctx context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
	return f(ctx, permit, scope)
}

func TestLegacyDirectAndTransactionMutationsRefuseProtectedRows(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	protected := &types.Issue{ID: "legacy-protected", Title: "before", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	ordinary := &types.Issue{ID: "legacy-ordinary", Title: "ordinary", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	for _, issue := range []*types.Issue{protected, ordinary} {
		if err := store.CreateIssue(ctx, issue, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", issue.ID, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", protected.ID, "source"); err != nil {
		t.Fatalf("classify issue: %v", err)
	}

	if err := store.UpdateIssue(ctx, protected.ID, map[string]interface{}{"title": "changed"}, "worker"); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("direct UpdateIssue error = %v, want ErrProtectedRecord", err)
	}
	if err := store.AddLabel(ctx, protected.ID, "blocked", "worker"); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("direct AddLabel error = %v, want ErrProtectedRecord", err)
	}
	if err := store.AddDependency(ctx, &types.Dependency{IssueID: ordinary.ID, DependsOnID: protected.ID, Type: types.DepBlocks}, "worker"); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("direct AddDependency touching protected endpoint error = %v, want ErrProtectedRecord", err)
	}

	err := store.RunInTransaction(ctx, "legacy protected transaction", func(tx storage.Transaction) error {
		return tx.UpdateIssue(ctx, protected.ID, map[string]interface{}{"title": "transaction-changed"}, "worker")
	})
	if !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("transaction UpdateIssue error = %v, want ErrProtectedRecord", err)
	}

	if err := store.UpdateIssue(ctx, ordinary.ID, map[string]interface{}{"title": "ordinary-updated"}, "worker"); err != nil {
		t.Fatalf("ordinary UpdateIssue: %v", err)
	}
	var title string
	if err := store.db.QueryRowContext(ctx, "SELECT title FROM issues WHERE id = ?", protected.ID).Scan(&title); err != nil {
		t.Fatalf("read protected title: %v", err)
	}
	if title != "before" {
		t.Fatalf("protected title after refused writes = %q, want before", title)
	}
	var labels, edges int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM labels WHERE issue_id = ?", protected.ID).Scan(&labels); err != nil {
		t.Fatalf("count protected labels: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dependencies WHERE issue_id = ? OR depends_on_issue_id = ?", ordinary.ID, protected.ID).Scan(&edges); err != nil {
		t.Fatalf("count touched dependency edges: %v", err)
	}
	if labels != 0 || edges != 0 {
		t.Fatalf("refused writes left labels=%d, dependency edges=%d; want both zero", labels, edges)
	}
}

func TestPermitAwareMutationConsumesOnceAndContextCannotAuthorizeNextTransaction(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{ID: "legacy-permit-context", Title: "before", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := store.CreateIssue(ctx, issue, "seed"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", issue.ID, "source"); err != nil {
		t.Fatalf("classify issue: %v", err)
	}

	verifier := protectedMutationVerifierFunc(func(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		if permit != "signed-test-permit" || scope.Operation != "issue.update" || len(scope.ResourceIDs) != 1 || scope.ResourceIDs[0] != issue.ID {
			t.Fatalf("unexpected permit scope: permit=%q scope=%#v", permit, scope)
		}
		return publicops.ProtectedMutationVerification{ReplayID: "legacy-permit-use-once"}, nil
	})
	ctx = issueops.WithProtectedMutationVerifier(ctx, verifier, "test-audience")
	lifecycle, err := store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle: %v", err)
	}
	_, err = lifecycle.Update(ctx, publicops.UpdateRequest{
		Actor:           "worker",
		IssueID:         issue.ID,
		ProtectedPermit: "signed-test-permit",
		Patch:           publicops.IssuePatch{Title: publicops.Field[string]{Set: true, Value: "authorized"}},
	})
	if err != nil {
		t.Fatalf("permit-aware Update: %v", err)
	}

	var uses int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?", "test-audience", "legacy-permit-use-once").Scan(&uses); err != nil {
		t.Fatalf("count replay-use rows: %v", err)
	}
	if uses != 1 {
		t.Fatalf("replay-use rows = %d, want exactly one", uses)
	}
	if err := store.AddLabel(ctx, issue.ID, "must-refuse", "worker"); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("later legacy AddLabel with reused context error = %v, want ErrProtectedRecord", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?", "test-audience", "legacy-permit-use-once").Scan(&uses); err != nil {
		t.Fatalf("recount replay-use rows: %v", err)
	}
	if uses != 1 {
		t.Fatalf("replay-use rows after refused transaction = %d, want exactly one", uses)
	}
}

func TestProtectedBatchReceiptReplaySkipsPermitRevalidation(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()
	applier, err := store.BatchApplier()
	if err != nil {
		t.Fatalf("BatchApplier: %v", err)
	}
	request := publicops.ApplyBatchRequest{
		Actor:           "trusted-caller",
		ProtectedPermit: "permit-first",
		ReceiptID:       "legacy-protected-receipt",
		ForceIDPrefix:   true,
		Items: []publicops.ApplyItem{{
			Kind: publicops.ItemCreate,
			Create: &publicops.CreateItem{
				Issue: &types.Issue{
					ID: "legacy-protected-receipt-target", Title: "protected receipt",
					Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask,
				},
				ProtectionClass: "source",
			},
		}},
	}
	var verifierCalls int
	verifier := protectedMutationVerifierFunc(func(_ context.Context, permit publicops.ProtectedMutationPermit, scope publicops.ProtectedMutationScope) (publicops.ProtectedMutationVerification, error) {
		verifierCalls++
		if verifierCalls != 1 {
			return publicops.ProtectedMutationVerification{}, errors.New("receipt replay revalidated the permit")
		}
		if permit != request.ProtectedPermit || scope.Operation != "issue.batch_apply" || scope.Audience != "test-audience" {
			t.Errorf("first permit scope = permit %q, %#v", permit, scope)
		}
		return publicops.ProtectedMutationVerification{ReplayID: "legacy-protected-receipt-use"}, nil
	})
	protectedCtx := issueops.WithProtectedMutationVerifier(ctx, verifier, "test-audience")
	first, err := applier.ApplyBatch(protectedCtx, request)
	if err != nil {
		t.Fatalf("first receipt-bearing protected batch: %v", err)
	}
	if first.Replayed {
		t.Fatal("first receipt-bearing protected batch was marked replayed")
	}
	var uses int
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?",
		"test-audience", "legacy-protected-receipt-use").Scan(&uses); err != nil {
		t.Fatalf("count permit uses: %v", err)
	}
	if uses != 1 {
		t.Fatalf("permit-use rows = %d, want 1", uses)
	}

	retry := request
	retry.ProtectedPermit = "permit-not-revalidated"
	replayed, err := applier.ApplyBatch(protectedCtx, retry)
	if err != nil {
		t.Fatalf("receipt replay after permit consumption: %v", err)
	}
	if !replayed.Replayed {
		t.Fatal("protected receipt replay was not marked replayed")
	}
	want := first
	want.Replayed = true
	if !reflect.DeepEqual(replayed, want) {
		t.Errorf("protected replay result differs from original:\n got %#v\nwant %#v", replayed, want)
	}
	if verifierCalls != 1 {
		t.Errorf("protected verifier called %d times, want once", verifierCalls)
	}
	if err := store.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM protected_mutation_permit_uses WHERE audience = ? AND replay_id = ?",
		"test-audience", "legacy-protected-receipt-use").Scan(&uses); err != nil {
		t.Fatalf("recount permit uses: %v", err)
	}
	if uses != 1 {
		t.Errorf("permit-use rows after receipt replay = %d, want 1", uses)
	}
}
