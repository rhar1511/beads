package versioncontrolops

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/storage"
)

func TestOrderMergeConflictsForRevisionFencing(t *testing.T) {
	input := []storage.Conflict{
		{Field: "labels"},
		{Field: "dependencies"},
		{Field: ""},
		{Field: "issues"},
		{Field: "comments"},
	}
	got := orderMergeConflictsForRevisionFencing(input)
	if len(got) != len(input) {
		t.Fatalf("ordered conflict count = %d, want %d", len(got), len(input))
	}
	if got[0].Field != "" || got[1].Field != "issues" {
		t.Fatalf("issue conflicts must resolve first, got fields %q, %q", got[0].Field, got[1].Field)
	}
	if got[len(got)-1].Field != "dependencies" {
		t.Fatalf("dependency conflicts must resolve last, got %q", got[len(got)-1].Field)
	}
}

func TestRepairDependencyCascadeFencesOnlySurvivingSources(t *testing.T) {
	ctx := context.Background()
	db, mock := newMockDB(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `table` FROM dolt_constraint_violations WHERE num_violations > 0")).
		WillReturnRows(sqlmock.NewRows([]string{"table"}).AddRow("dependencies"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT violation_type, violation_info FROM dolt_constraint_violations_dependencies")).
		WillReturnRows(sqlmock.NewRows([]string{"violation_type", "violation_info"}).AddRow("foreign key", `{"ReferencedTable":"issues"}`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT d.issue_id, d.depends_on_issue_id, d.type
		FROM dependencies d
		JOIN issues source ON source.id = d.issue_id
		WHERE d.depends_on_issue_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM issues target WHERE target.id = d.depends_on_issue_id)
		ORDER BY d.issue_id, d.depends_on_issue_id, d.type`)).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id", "depends_on_issue_id", "type"}).AddRow("live-source", "deleted-target", "related"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT row_lock FROM issues WHERE id = ?")).
		WithArgs("live-source").WillReturnRows(sqlmock.NewRows([]string{"row_lock"}).AddRow(int64(42)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE issues SET row_lock = ?, updated_at = updated_at WHERE id = ?")).
		WithArgs(sqlmock.AnyArg(), "live-source").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(fkCascadeRepairDeletes["dependencies"])).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM dolt_constraint_violations_dependencies")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("CALL DOLT_ADD('dependencies')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("CALL DOLT_ADD('issues')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `table` FROM dolt_constraint_violations WHERE num_violations > 0")).
		WillReturnRows(sqlmock.NewRows([]string{"table"}))

	repaired, had, err := TryRepairFKCascadeViolations(ctx, db)
	if err != nil {
		t.Fatalf("TryRepairFKCascadeViolations: %v", err)
	}
	if !repaired || !had {
		t.Fatalf("TryRepairFKCascadeViolations = (%v, %v), want (true, true)", repaired, had)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestRepairDependencyCascadeDoesNotFenceDeletedSources(t *testing.T) {
	ctx := context.Background()
	db, mock := newMockDB(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `table` FROM dolt_constraint_violations WHERE num_violations > 0")).
		WillReturnRows(sqlmock.NewRows([]string{"table"}).AddRow("dependencies"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT violation_type, violation_info FROM dolt_constraint_violations_dependencies")).
		WillReturnRows(sqlmock.NewRows([]string{"violation_type", "violation_info"}).AddRow("foreign key", `{"ReferencedTable":"issues"}`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT d.issue_id, d.depends_on_issue_id, d.type
		FROM dependencies d
		JOIN issues source ON source.id = d.issue_id
		WHERE d.depends_on_issue_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM issues target WHERE target.id = d.depends_on_issue_id)
		ORDER BY d.issue_id, d.depends_on_issue_id, d.type`)).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id", "depends_on_issue_id", "type"}))
	mock.ExpectExec(regexp.QuoteMeta(fkCascadeRepairDeletes["dependencies"])).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM dolt_constraint_violations_dependencies")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("CALL DOLT_ADD('dependencies')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `table` FROM dolt_constraint_violations WHERE num_violations > 0")).
		WillReturnRows(sqlmock.NewRows([]string{"table"}))

	repaired, had, err := TryRepairFKCascadeViolations(ctx, db)
	if err != nil {
		t.Fatalf("TryRepairFKCascadeViolations: %v", err)
	}
	if !repaired || !had {
		t.Fatalf("TryRepairFKCascadeViolations = (%v, %v), want (true, true)", repaired, had)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestResolveDependencyConflictsStagesRemintedSourceRevision(t *testing.T) {
	ctx := context.Background()
	db, mock := newMockDB(t)
	mock.ExpectQuery(`(?s)SELECT our_issue_id, their_issue_id,.*FROM dolt_conflicts_dependencies`).
		WillReturnRows(sqlmock.NewRows([]string{
			"our_issue_id", "their_issue_id",
			"our_depends_on_issue_id", "their_depends_on_issue_id",
			"our_depends_on_wisp_id", "their_depends_on_wisp_id",
			"our_depends_on_external", "their_depends_on_external",
			"our_type", "their_type",
		}).AddRow("source", "source", "target-a", "target-b", nil, nil, nil, nil, "related", "related"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM issues WHERE id = ?")).
		WithArgs("source").WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("CALL DOLT_CONFLICTS_RESOLVE('--theirs', 'dependencies')")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT row_lock FROM issues WHERE id = ?")).
		WithArgs("source").WillReturnRows(sqlmock.NewRows([]string{"row_lock"}).AddRow(int64(90)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE issues SET row_lock = ?, updated_at = updated_at WHERE id = ?")).
		WithArgs(sqlmock.AnyArg(), "source").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("CALL DOLT_ADD('issues')")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ResolveConflicts(ctx, db, "dependencies", "theirs"); err != nil {
		t.Fatalf("ResolveConflicts: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
