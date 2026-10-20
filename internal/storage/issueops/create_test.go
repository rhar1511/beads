package issueops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rhar1511/beads/internal/storage"
	"github.com/rhar1511/beads/internal/storage/depid"
	"github.com/rhar1511/beads/internal/types"
)

func expectIssueRevisionBump(mock sqlmock.Sqlmock, issueID string) {
	mock.ExpectQuery("SELECT row_lock FROM issues WHERE id = \\?").
		WithArgs(issueID).
		WillReturnRows(sqlmock.NewRows([]string{"row_lock"}).AddRow(int64(17)))
	mock.ExpectExec("UPDATE issues SET row_lock = \\?, updated_at = updated_at WHERE id = \\?").
		WithArgs(sqlmock.AnyArg(), issueID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectDependencyIdentity(mock sqlmock.Sqlmock, table, targetColumn, source, target string, count int) {
	mock.ExpectQuery(regexp.QuoteMeta(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE issue_id = ? AND %s = ?", table, targetColumn))).
		WithArgs(source, target).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(count))
}

func TestValidateCreateIssuesMixedBucketDependenciesRejectsCrossBucketEdges(t *testing.T) {
	regularA := &types.Issue{ID: "test-regular-a", IssueType: types.TypeTask}
	regularB := &types.Issue{ID: "test-regular-b", IssueType: types.TypeTask}
	wispA := &types.Issue{ID: "test-wisp-a", IssueType: types.TypeTask, Ephemeral: true}
	wispB := &types.Issue{ID: "test-wisp-b", IssueType: types.TypeTask, Ephemeral: true}

	tests := []struct {
		name      string
		issues    []*types.Issue
		wantError bool
	}{
		{
			name: "regular to wisp",
			issues: []*types.Issue{
				{
					ID:        regularA.ID,
					IssueType: types.TypeTask,
					Dependencies: []*types.Dependency{{
						DependsOnID: wispA.ID,
						Type:        types.DepBlocks,
					}},
				},
				wispA,
			},
			wantError: true,
		},
		{
			name: "wisp to regular",
			issues: []*types.Issue{
				regularA,
				{
					ID:        wispA.ID,
					IssueType: types.TypeTask,
					Ephemeral: true,
					Dependencies: []*types.Dependency{{
						DependsOnID: regularA.ID,
						Type:        types.DepBlocks,
					}},
				},
			},
			wantError: true,
		},
		{
			name: "same bucket dependencies",
			issues: []*types.Issue{
				regularB,
				{
					ID:        regularA.ID,
					IssueType: types.TypeTask,
					Dependencies: []*types.Dependency{{
						DependsOnID: regularB.ID,
						Type:        types.DepBlocks,
					}},
				},
				wispB,
				{
					ID:        wispA.ID,
					IssueType: types.TypeTask,
					Ephemeral: true,
					Dependencies: []*types.Dependency{{
						DependsOnID: wispB.ID,
						Type:        types.DepBlocks,
					}},
				},
			},
		},
		{
			name: "out of batch target",
			issues: []*types.Issue{
				{
					ID:        regularA.ID,
					IssueType: types.TypeTask,
					Dependencies: []*types.Dependency{{
						DependsOnID: "test-external-wisp",
						Type:        types.DepBlocks,
					}},
				},
				wispA,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreateIssuesMixedBucketDependencies(tt.issues)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "cross-bucket dependency") {
					t.Fatalf("error = %v, want cross-bucket dependency", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
		})
	}
}

func TestPersistDependenciesHonorsImportedCreatedBy(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()

	target := &types.Issue{ID: "target", IssueType: types.TypeTask}
	source := &types.Issue{
		ID:        "source",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{
			DependsOnID: "target",
			Type:        types.DepRelated,
			CreatedBy:   "someone.else",
		}},
	}

	mock.ExpectQuery("SELECT 1 FROM wisps WHERE id = \\? LIMIT 1").
		WithArgs("target").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").
		WithArgs("target").
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	expectDependencyIdentity(mock, "dependencies", "depends_on_issue_id", "source", "target", 0)
	mock.ExpectExec("INSERT INTO dependencies").
		WithArgs(depid.New("source", "target"), "source", "target", types.DepRelated, "someone.else", sqlmock.AnyArg(), "{}", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectIssueRevisionBump(mock, "source")

	result, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{target, source}, "current.user", storage.BatchCreateOptions{})
	if err != nil {
		t.Fatalf("PersistDependenciesWithOptionsResult error = %v, want nil", err)
	}
	if !result.ChangedTables["dependencies"] {
		t.Fatalf("ChangedTables = %#v, want dependencies changed", result.ChangedTables)
	}
	if !result.ChangedTables["issues"] {
		t.Fatalf("ChangedTables = %#v, want issues staged with the source revision", result.ChangedTables)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPersistDependenciesRejectsForeignSourceOwner(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()

	issue := &types.Issue{
		ID:        "created-owner",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{
			IssueID:     "different-owner",
			DependsOnID: "target",
			Type:        types.DepRelated,
		}},
	}
	_, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{issue}, "tester", storage.BatchCreateOptions{})
	if !errors.Is(err, storage.ErrValidation) {
		t.Fatalf("PersistDependenciesWithOptionsResult error = %v, want ErrValidation", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (foreign source must fail before any SQL): %v", err)
	}
}

func TestPersistDependenciesBumpsExistingOwnerOnlyForInsertedEdge(t *testing.T) {
	ctx := context.Background()
	t.Run("inserted edge advances source and stages issues", func(t *testing.T) {
		db, mock, tx := beginMockTx(t)
		defer db.Close()
		issue := &types.Issue{
			ID:        "existing-source",
			IssueType: types.TypeTask,
			Dependencies: []*types.Dependency{{
				DependsOnID: "target",
				Type:        types.DepRelated,
			}},
		}
		expectDependencyIdentity(mock, "dependencies", "depends_on_external", "existing-source", "target", 0)
		mock.ExpectExec("INSERT INTO dependencies").
			WithArgs(depid.New("existing-source", "target"), "existing-source", "target", types.DepRelated, "tester", sqlmock.AnyArg(), "{}", "").
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectIssueRevisionBump(mock, "existing-source")

		result, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{issue}, "tester", storage.BatchCreateOptions{})
		if err != nil {
			t.Fatalf("PersistDependenciesWithOptionsResult error = %v, want nil", err)
		}
		if !result.ChangedTables["issues"] || !result.ChangedTables["dependencies"] {
			t.Fatalf("ChangedTables = %#v, want issues and dependencies", result.ChangedTables)
		}
		mock.ExpectRollback()
		if err := tx.Rollback(); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	for _, matchedRows := range []int64{0, 1} {
		t.Run(fmt.Sprintf("idempotent edge matched rows %d", matchedRows), func(t *testing.T) {
			db, mock, tx := beginMockTx(t)
			defer db.Close()
			issue := &types.Issue{
				ID:        "existing-source",
				IssueType: types.TypeTask,
				Dependencies: []*types.Dependency{{
					DependsOnID: "target",
					Type:        types.DepRelated,
				}},
			}
			expectDependencyIdentity(mock, "dependencies", "depends_on_external", "existing-source", "target", 1)
			mock.ExpectExec("INSERT INTO dependencies").
				WithArgs(depid.New("existing-source", "target"), "existing-source", "target", types.DepRelated, "tester", sqlmock.AnyArg(), "{}", "").
				WillReturnResult(sqlmock.NewResult(0, matchedRows))

			result, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{issue}, "tester", storage.BatchCreateOptions{})
			if err != nil {
				t.Fatalf("PersistDependenciesWithOptionsResult error = %v, want nil", err)
			}
			if result.ChangedTables["issues"] || result.ChangedTables["dependencies"] {
				t.Fatalf("ChangedTables = %#v, want no durable graph changes", result.ChangedTables)
			}
			mock.ExpectRollback()
			if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
		})
	}
}

func TestPersistDependenciesDefaultsCreatedByToActor(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()

	target := &types.Issue{ID: "target", IssueType: types.TypeTask}
	source := &types.Issue{
		ID:        "source",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{
			DependsOnID: "target",
			Type:        types.DepRelated,
		}},
	}

	mock.ExpectQuery("SELECT 1 FROM wisps WHERE id = \\? LIMIT 1").
		WithArgs("target").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").
		WithArgs("target").
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	expectDependencyIdentity(mock, "dependencies", "depends_on_issue_id", "source", "target", 0)
	mock.ExpectExec("INSERT INTO dependencies").
		WithArgs(depid.New("source", "target"), "source", "target", types.DepRelated, "current.user", sqlmock.AnyArg(), "{}", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectIssueRevisionBump(mock, "source")

	_, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{target, source}, "current.user", storage.BatchCreateOptions{})
	if err != nil {
		t.Fatalf("PersistDependenciesWithOptionsResult error = %v, want nil", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPersistDependenciesClassifiesBareCrossPrefixTargetAsExternal(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()

	source := &types.Issue{
		ID:        "sym-3su",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{
			DependsOnID: "mkt-456",
			Type:        types.DepRelated,
		}},
	}
	var skipped []string

	// A bare target with a different issue prefix is external. In particular,
	// persistence must not probe either local target table before this insert.
	expectDependencyIdentity(mock, "dependencies", "depends_on_external", "sym-3su", "mkt-456", 0)
	mock.ExpectExec("INSERT INTO dependencies \\(id, issue_id, depends_on_external").
		WithArgs(depid.New("sym-3su", "mkt-456"), "sym-3su", "mkt-456", types.DepRelated, "tester", sqlmock.AnyArg(), "{}", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectIssueRevisionBump(mock, "sym-3su")

	result, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{source}, "tester", storage.BatchCreateOptions{
		OnSkippedDependency: func(issueID, dependsOnID, reason string) {
			skipped = append(skipped, issueID+" -> "+dependsOnID+": "+reason)
		},
	})
	if err != nil {
		t.Fatalf("PersistDependenciesWithOptionsResult error = %v, want nil", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %#v, want none", skipped)
	}
	if !result.ChangedTables["dependencies"] {
		t.Fatalf("ChangedTables = %#v, want dependencies changed", result.ChangedTables)
	}
	if !result.ChangedTables["issues"] {
		t.Fatalf("ChangedTables = %#v, want issues staged with the source revision", result.ChangedTables)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPersistDependenciesReturnsTargetLookupErrors(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()
	targetErr := errors.New("target lookup failed")
	issue := &types.Issue{
		ID:        "source",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{
			DependsOnID: "target",
			Type:        types.DepBlocks,
		}},
	}

	mock.ExpectQuery("SELECT 1 FROM wisps WHERE id = \\? LIMIT 1").
		WithArgs("target").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").
		WithArgs("target").
		WillReturnError(targetErr)

	err := PersistDependencies(ctx, tx, []*types.Issue{issue}, "tester")
	if err == nil || !strings.Contains(err.Error(), "failed to check dependency target target for source") {
		t.Fatalf("error = %v, want dependency target lookup error", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPersistDependenciesSkipsValidationErrorsWhenConfigured(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()
	issue := &types.Issue{
		ID:        "source",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{
			DependsOnID: "source",
			Type:        types.DepBlocks,
		}},
	}
	var skipped []string

	mock.ExpectQuery("SELECT 1 FROM wisps WHERE id = \\? LIMIT 1").
		WithArgs("source").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").
		WithArgs("source").
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))

	result, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{issue}, "tester", storage.BatchCreateOptions{
		SkipDependencyValidationErrors: true,
		OnSkippedDependency: func(issueID, dependsOnID, reason string) {
			skipped = append(skipped, issueID+" -> "+dependsOnID+": "+reason)
		},
	})
	if err != nil {
		t.Fatalf("PersistDependenciesWithOptionsResult error = %v, want nil", err)
	}
	if len(result.ChangedTables) != 0 {
		t.Fatalf("ChangedTables = %#v, want none", result.ChangedTables)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "source -> source") ||
		!strings.Contains(skipped[0], "cannot depend on itself") {
		t.Fatalf("skipped = %#v, want self-dependency detail", skipped)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPersistDependenciesRejectsHierarchyBlocking(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()
	issue := &types.Issue{
		ID:        "child",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{
			DependsOnID: "parent",
			Type:        types.DepConditionalBlocks,
		}},
	}

	mock.ExpectQuery("SELECT 1 FROM wisps WHERE id = \\? LIMIT 1").
		WithArgs("parent").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").
		WithArgs("parent").
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery("WITH RECURSIVE ancestors").
		WithArgs("child", "parent").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	_, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{issue}, "tester", storage.BatchCreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "cannot be blocked by its ancestor") {
		t.Fatalf("error = %v, want ancestor hierarchy rejection", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPersistDependenciesValidatesPlannedHierarchyBeforeBlocking(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()
	child := &types.Issue{
		ID:        "bd-child",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{
			{DependsOnID: "bd-grand", Type: types.DepBlocks}, // Deliberately first.
			{DependsOnID: "bd-parent", Type: types.DepParentChild},
		},
	}
	parent := &types.Issue{
		ID:        "bd-parent",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{
			DependsOnID: "bd-grand",
			Type:        types.DepParentChild,
		}},
	}

	for _, pair := range [][2]string{{"bd-child", "bd-parent"}, {"bd-parent", "bd-grand"}} {
		mock.ExpectQuery("SELECT 1 FROM wisps WHERE id = \\? LIMIT 1").
			WithArgs(pair[1]).
			WillReturnError(sql.ErrNoRows)
		mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").
			WithArgs(pair[1]).
			WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
		mock.ExpectQuery("WITH RECURSIVE reachable").
			WithArgs(pair[1], pair[0]).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		expectDependencyIdentity(mock, "dependencies", "depends_on_issue_id", pair[0], pair[1], 0)
		mock.ExpectExec("INSERT INTO dependencies").
			WithArgs(depid.New(pair[0], pair[1]), pair[0], pair[1], types.DepParentChild, "tester", sqlmock.AnyArg(), "{}", "").
			WillReturnResult(sqlmock.NewResult(0, 1))
		expectIssueRevisionBump(mock, pair[0])
		mock.ExpectExec("REPLACE INTO local_metadata").
			WithArgs(dependencyCoordinationKey(pair[1], dependencyCoordinationDurableTier), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectQuery("SELECT 1 FROM wisps WHERE id = \\? LIMIT 1").
		WithArgs("bd-grand").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").
		WithArgs("bd-grand").
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery("WITH RECURSIVE ancestors").
		WithArgs("bd-child", "bd-grand").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	_, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{child, parent}, "tester", storage.BatchCreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "cannot be blocked by its ancestor") {
		t.Fatalf("error = %v, want planned-ancestor rejection", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPersistDependenciesSkipsHierarchyValidationAcrossPrefixes(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()
	issue := &types.Issue{
		ID:        "aa-source",
		IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{
			DependsOnID: "bb-target",
			Type:        types.DepBlocks,
		}},
	}

	// No target or ancestors query: target existence and hierarchy cannot be
	// validated locally across rig prefixes.
	mock.ExpectQuery("WITH RECURSIVE reachable").
		WithArgs("bb-target", "aa-source").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	expectDependencyIdentity(mock, "dependencies", "depends_on_external", "aa-source", "bb-target", 0)
	mock.ExpectExec("INSERT INTO dependencies \\(id, issue_id, depends_on_external").
		WithArgs(depid.New("aa-source", "bb-target"), "aa-source", "bb-target", types.DepBlocks, "tester", sqlmock.AnyArg(), "{}", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectIssueRevisionBump(mock, "aa-source")

	_, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{issue}, "tester", storage.BatchCreateOptions{})
	if err != nil {
		t.Fatalf("cross-prefix blocking dependency: %v", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// The mock half of the missing-parent skip: it pins the STATEMENTS (no counter
// read, no upsert). Its real-backend twin is the conformance case
// ReconcileSkipsMissingParentCounter (backend/conformance/portable.go), which
// pins what a live engine does with the same skip on both Dolt legs.
func TestReconcileChildCountersSkipsMissingParent(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()

	mock.ExpectQuery("SELECT 1 FROM wisps LIMIT 1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").
		WithArgs("test-deleted-parent").
		WillReturnError(sql.ErrNoRows)

	changed, err := ReconcileChildCounters(ctx, tx, []*types.Issue{{
		ID:        "test-deleted-parent.7",
		IssueType: types.TypeTask,
	}})
	if err != nil {
		t.Fatalf("ReconcileChildCounters error = %v, want nil", err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed tables = %#v, want none", changed)
	}

	// No counter SELECT or upsert is expected after the missing-parent lookup.
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestReconcileChildCountersReturnsParentLookupError(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()
	lookupErr := errors.New("parent lookup failed")

	mock.ExpectQuery("SELECT 1 FROM wisps LIMIT 1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").
		WithArgs("test-parent").
		WillReturnError(lookupErr)

	_, err := ReconcileChildCounters(ctx, tx, []*types.Issue{{
		ID:        "test-parent.1",
		IssueType: types.TypeTask,
	}})
	if err == nil || !strings.Contains(err.Error(), "failed to check child counter parent test-parent") || !errors.Is(err, lookupErr) {
		t.Fatalf("error = %v, want contextual parent lookup error", err)
	}

	// A lookup failure must not be mistaken for an absent parent or reach the
	// counter table.
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestReconcileChildCountersReturnsWispLookupError(t *testing.T) {
	ctx := context.Background()
	db, mock, tx := beginMockTx(t)
	defer db.Close()
	lookupErr := errors.New("wisp lookup failed")

	mock.ExpectQuery("SELECT 1 FROM wisps LIMIT 1").
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery("SELECT id FROM wisps WHERE id IN \\(\\?\\)").
		WithArgs("test-parent").
		WillReturnError(lookupErr)

	_, err := ReconcileChildCounters(ctx, tx, []*types.Issue{{
		ID:        "test-parent.1",
		IssueType: types.TypeTask,
	}})
	if err == nil || !strings.Contains(err.Error(), "failed to route child counter parents") || !errors.Is(err, lookupErr) {
		t.Fatalf("error = %v, want contextual wisp lookup error", err)
	}

	// A failed wisp lookup must stop routing before any issues or counter query.
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPersistReverseDependenciesBumpsWispOwnerOnlyForInsertedEdge(t *testing.T) {
	for _, inserted := range []bool{true, false} {
		name := "exact duplicate preserves token"
		if inserted {
			name = "inserted edge remints token"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			db, mock, tx := beginMockTx(t)
			defer db.Close()
			issue := &types.Issue{ID: "test-created", IssueType: types.TypeTask,
				Dependencies: []*types.Dependency{{IssueID: "test-owner", DependsOnID: "test-created", Type: types.DepRelatesTo}}}
			mock.ExpectQuery("SELECT 1 FROM wisps WHERE id = \\? LIMIT 1").WithArgs("test-owner").
				WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
			mock.ExpectQuery("SELECT 1 FROM wisps WHERE id = \\? LIMIT 1").WithArgs("test-created").WillReturnError(sql.ErrNoRows)
			mock.ExpectQuery("SELECT 1 FROM issues WHERE id = \\?").WithArgs("test-created").
				WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
			existingCount := 1
			if inserted {
				existingCount = 0
			}
			expectDependencyIdentity(mock, "wisp_dependencies", "depends_on_issue_id", "test-owner", "test-created", existingCount)
			mock.ExpectExec("INSERT INTO wisp_dependencies").
				WithArgs(depid.New("test-owner", "test-created"), "test-owner", "test-created", types.DepRelatesTo, "writer", sqlmock.AnyArg(), "{}", "").
				WillReturnResult(sqlmock.NewResult(0, 1))
			if inserted {
				mock.ExpectQuery("SELECT row_lock FROM wisps WHERE id = \\?").WithArgs("test-owner").
					WillReturnRows(sqlmock.NewRows([]string{"row_lock"}).AddRow(int64(17)))
				mock.ExpectExec("UPDATE wisps SET row_lock = \\?, updated_at = updated_at WHERE id = \\?").
					WithArgs(sqlmock.AnyArg(), "test-owner").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			result, err := PersistDependenciesWithOptionsResult(ctx, tx, []*types.Issue{issue}, "writer", storage.BatchCreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if result.ChangedTables["wisp_dependencies"] != inserted || result.ChangedTables["issues"] {
				t.Fatalf("changed tables = %#v, want wisp edge changed=%v without durable issue stage", result.ChangedTables, inserted)
			}
			mock.ExpectRollback()
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPersistDuplicateDependencyRetainsStrictInsertError(t *testing.T) {
	db, mock, tx := beginMockTx(t)
	defer db.Close()
	issue := &types.Issue{ID: "existing-source", IssueType: types.TypeTask,
		Dependencies: []*types.Dependency{{DependsOnID: "target", Type: types.DepRelated, Metadata: "invalid-json"}}}
	expectDependencyIdentity(mock, "dependencies", "depends_on_external", "existing-source", "target", 1)
	insertErr := errors.New("strict duplicate payload rejected")
	mock.ExpectExec("INSERT INTO dependencies").
		WithArgs(depid.New("existing-source", "target"), "existing-source", "target", types.DepRelated, "tester", sqlmock.AnyArg(), "invalid-json", "").
		WillReturnError(insertErr)
	result, err := PersistDependenciesWithOptionsResult(t.Context(), tx, []*types.Issue{issue}, "tester", storage.BatchCreateOptions{})
	if !errors.Is(err, insertErr) {
		t.Fatalf("duplicate error = %v, want original strict insert error", err)
	}
	if len(result.ChangedTables) != 0 {
		t.Fatalf("refused duplicate changed tables = %#v", result.ChangedTables)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
