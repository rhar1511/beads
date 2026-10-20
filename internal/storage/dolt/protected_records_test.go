package dolt

import (
	"errors"
	"testing"

	"github.com/rhar1511/beads/internal/types"
	publicops "github.com/rhar1511/beads/issueops"
)

func TestProtectedRecordDeleteRefusesBeforeAnyMutation(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	root := &types.Issue{ID: "protected-root", Title: "root", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	child := &types.Issue{ID: "protected-child", Title: "child", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	for _, issue := range []*types.Issue{root, child} {
		if err := store.CreateIssue(ctx, issue, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", issue.ID, err)
		}
	}
	if err := store.AddDependency(ctx, &types.Dependency{IssueID: child.ID, DependsOnID: root.ID, Type: types.DepParentChild}, "seed"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", root.ID, "source"); err != nil {
		t.Fatalf("classify root: %v", err)
	}

	deleter, err := store.Deleter()
	if err != nil {
		t.Fatalf("Deleter: %v", err)
	}
	for _, req := range []publicops.DeleteRequest{
		{IDs: []string{root.ID}, Force: true},
		{IDs: []string{root.ID}, DryRun: true},
		{IDs: []string{child.ID}, Cascade: true}, // protected descendant is in the resolved closure
		{IDs: []string{child.ID, root.ID}, Force: true},
	} {
		if _, err := deleter.Delete(ctx, req); !errors.Is(err, publicops.ErrProtectedRecord) {
			t.Fatalf("Delete(%+v) error = %v, want ErrProtectedRecord", req, err)
		}
	}

	for _, id := range []string{root.ID, child.ID} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues WHERE id = ?", id).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if count != 1 {
			t.Errorf("issue %s count = %d after refused delete, want 1", id, count)
		}
	}
}

func TestProtectedRecordDeleteRefusesProtectedSurvivingDependent(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	target := &types.Issue{ID: "protected-survivor-target", Title: "target", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	dependent := &types.Issue{ID: "protected-survivor-dependent", Title: "dependent", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	for _, issue := range []*types.Issue{target, dependent} {
		if err := store.CreateIssue(ctx, issue, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", issue.ID, err)
		}
	}
	if err := store.AddDependency(ctx, &types.Dependency{IssueID: dependent.ID, DependsOnID: target.ID, Type: types.DepBlocks}, "seed"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", dependent.ID, "source"); err != nil {
		t.Fatalf("classify dependent: %v", err)
	}

	deleter, err := store.Deleter()
	if err != nil {
		t.Fatalf("Deleter: %v", err)
	}
	if _, err := deleter.Delete(ctx, publicops.DeleteRequest{IDs: []string{target.ID}, Force: true}); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("Delete target error = %v, want ErrProtectedRecord", err)
	}

	for _, id := range []string{target.ID, dependent.ID} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM issues WHERE id = ?", id).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if count != 1 {
			t.Errorf("issue %s count = %d after refused delete, want 1", id, count)
		}
	}
	var edgeCount int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dependencies WHERE issue_id = ? AND depends_on_id = ?", dependent.ID, target.ID).Scan(&edgeCount); err != nil {
		t.Fatalf("count dependency: %v", err)
	}
	if edgeCount != 1 {
		t.Fatalf("dependency count after refused delete = %d, want 1", edgeCount)
	}
}

func TestSourceRepoDeleteRefusesProtectedSurvivingNeighborBeforeRevisionBump(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	target := &types.Issue{ID: "protected-repo-target", Title: "target", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, SourceRepo: "repo-delete"}
	survivor := &types.Issue{ID: "protected-repo-survivor", Title: "survivor", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, SourceRepo: "repo-keep"}
	for _, issue := range []*types.Issue{target, survivor} {
		if err := store.CreateIssue(ctx, issue, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", issue.ID, err)
		}
	}
	if err := store.AddDependency(ctx, &types.Dependency{IssueID: survivor.ID, DependsOnID: target.ID, Type: types.DepRelated}, "seed"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", survivor.ID, "source"); err != nil {
		t.Fatalf("classify survivor: %v", err)
	}
	var revisionBefore int64
	if err := store.db.QueryRowContext(ctx, "SELECT row_lock FROM issues WHERE id = ?", survivor.ID).Scan(&revisionBefore); err != nil {
		t.Fatalf("read survivor revision: %v", err)
	}

	if _, err := store.DeleteIssuesBySourceRepo(ctx, target.SourceRepo); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("DeleteIssuesBySourceRepo error = %v, want ErrProtectedRecord", err)
	}
	for _, id := range []string{target.ID, survivor.ID} {
		if _, err := store.GetIssue(ctx, id); err != nil {
			t.Errorf("GetIssue(%s) after refused source-repo delete: %v", id, err)
		}
	}
	var revisionAfter int64
	if err := store.db.QueryRowContext(ctx, "SELECT row_lock FROM issues WHERE id = ?", survivor.ID).Scan(&revisionAfter); err != nil {
		t.Fatalf("reread survivor revision: %v", err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("protected survivor revision changed from %d to %d", revisionBefore, revisionAfter)
	}
}

func TestProtectedRecordDeleteRefusesProtectedRewrittenNeighbor(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	target := &types.Issue{ID: "protected-rewrite-target", Title: "target", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	neighbor := &types.Issue{ID: "protected-rewrite-neighbor", Title: "neighbor", Description: "See protected-rewrite-target before changing this", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	for _, issue := range []*types.Issue{target, neighbor} {
		if err := store.CreateIssue(ctx, issue, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", issue.ID, err)
		}
	}
	if err := store.AddDependency(ctx, &types.Dependency{IssueID: neighbor.ID, DependsOnID: target.ID, Type: types.DepRelated}, "seed"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", neighbor.ID, "source"); err != nil {
		t.Fatalf("classify neighbor: %v", err)
	}

	deleter, err := store.Deleter()
	if err != nil {
		t.Fatalf("Deleter: %v", err)
	}
	if _, err := deleter.Delete(ctx, publicops.DeleteRequest{IDs: []string{target.ID}, Force: true}); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("Delete target error = %v, want ErrProtectedRecord", err)
	}

	var description string
	if err := store.db.QueryRowContext(ctx, "SELECT description FROM issues WHERE id = ?", neighbor.ID).Scan(&description); err != nil {
		t.Fatalf("read neighbor description: %v", err)
	}
	if description != neighbor.Description {
		t.Fatalf("neighbor description after refused delete = %q, want %q", description, neighbor.Description)
	}
	if _, err := store.GetIssue(ctx, target.ID); err != nil {
		t.Fatalf("target after refused delete: %v", err)
	}
}

func TestLegacyMixedDeleteRefusesProtectedRegularBeforeDeletingWisp(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	wisp := &types.Issue{ID: "protected-mixed-wisp", Title: "wisp", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true}
	regular := &types.Issue{ID: "protected-mixed-regular", Title: "regular", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := store.CreateIssue(ctx, wisp, "seed"); err != nil {
		t.Fatalf("CreateIssue(wisp): %v", err)
	}
	if err := store.CreateIssue(ctx, regular, "seed"); err != nil {
		t.Fatalf("CreateIssue(regular): %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", regular.ID, "source"); err != nil {
		t.Fatalf("classify regular issue: %v", err)
	}

	if _, err := store.DeleteIssues(ctx, []string{wisp.ID, regular.ID}, false, true, false); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("DeleteIssues mixed set error = %v, want ErrProtectedRecord", err)
	}
	for _, id := range []string{wisp.ID, regular.ID} {
		if _, err := store.GetIssue(ctx, id); err != nil {
			t.Errorf("GetIssue(%s) after refused mixed delete: %v", id, err)
		}
	}
}

func TestStandaloneWispBatchRefusesWholeSetBeforeDeletingAnyWisp(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	first := &types.Issue{ID: "protected-batch-wisp-first", Title: "first", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true}
	protected := &types.Issue{ID: "protected-batch-wisp-later", Title: "later", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: true}
	for _, issue := range []*types.Issue{first, protected} {
		if err := store.CreateIssue(ctx, issue, "seed"); err != nil {
			t.Fatalf("CreateIssue(%s): %v", issue.ID, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", protected.ID, "source"); err != nil {
		t.Fatalf("classify later wisp: %v", err)
	}

	if _, err := store.deleteWispBatch(ctx, []string{first.ID, protected.ID}); !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("deleteWispBatch error = %v, want ErrProtectedRecord", err)
	}
	for _, id := range []string{first.ID, protected.ID} {
		if _, err := store.GetIssue(ctx, id); err != nil {
			t.Errorf("GetIssue(%s) after refused batch: %v", id, err)
		}
	}
}

func TestQ43ReceiptDoesNotImplyProtectedClassification(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{ID: "receipt-only", Title: "ordinary", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := store.CreateIssue(ctx, issue, "seed"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO issue_transition_receipts
		(receipt_id, issue_id, scope, kind, actor, metadata_key, from_revision, to_revision,
		expected_present, expected_json, value_present, value_json, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"receipt-only-id", issue.ID, "arbitrary-scope", "test", "caller", "marker", 0, 1,
		0, nil, 1, `true`, `{}`); err != nil {
		t.Fatalf("insert Q43 receipt: %v", err)
	}

	deleter, err := store.Deleter()
	if err != nil {
		t.Fatalf("Deleter: %v", err)
	}
	result, err := deleter.Delete(ctx, publicops.DeleteRequest{IDs: []string{issue.ID}, Force: true})
	if err != nil {
		t.Fatalf("Delete issue with receipt but no classification: %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", result.Deleted)
	}
}

func TestProtectedRecordLifecycleMutationRequiresPermit(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	issue := &types.Issue{ID: "protected-update", Title: "before", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
	if err := store.CreateIssue(ctx, issue, "seed"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO protected_issue_records (issue_id, protection_class) VALUES (?, ?)", issue.ID, "source"); err != nil {
		t.Fatalf("classify issue: %v", err)
	}

	lifecycle, err := store.IssueLifecycle()
	if err != nil {
		t.Fatalf("IssueLifecycle: %v", err)
	}
	_, err = lifecycle.Update(ctx, publicops.UpdateRequest{
		Actor:   "ordinary-worker",
		IssueID: issue.ID,
		Patch:   publicops.IssuePatch{Title: publicops.Field[string]{Set: true, Value: "after"}},
	})
	if !errors.Is(err, publicops.ErrProtectedRecord) {
		t.Fatalf("Update error = %v, want ErrProtectedRecord", err)
	}
	var title string
	if err := store.db.QueryRowContext(ctx, "SELECT title FROM issues WHERE id = ?", issue.ID).Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if title != "before" {
		t.Fatalf("title after refused update = %q, want before", title)
	}
}
