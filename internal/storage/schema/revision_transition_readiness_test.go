package schema

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

var errMissingReadinessTable = errors.New("receipt table absent")

func TestRevisionTransitionReadinessRequiresBothMigrationPlanes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mainVersion int
		ignoredVer  int
	}{
		{name: "main aggregate backfill absent", mainVersion: 67, ignoredVer: RevisionTransitionIgnoredMigration},
		{name: "ignored zero-token backfill absent", mainVersion: RevisionTransitionMainMigration, ignoredVer: 26},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectReadinessVersions(mock, tc.mainVersion, tc.ignoredVer)
			readiness, err := ProbeRevisionTransitionReadiness(context.Background(), db)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if readiness.Ready() {
				t.Fatal("partial migration state reported ready")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestZeroRevisionTransitionReadinessIsUnknown(t *testing.T) {
	var readiness RevisionTransitionReadiness
	if readiness.Ready() {
		t.Fatal("zero-value readiness proof is ready")
	}
}

func TestRevisionTransitionReadinessRequiresBackfillAndReceiptTable(t *testing.T) {
	for _, table := range []string{"issues", "wisps"} {
		t.Run("zero aggregate token remains in "+table, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectReadinessVersions(mock, RevisionTransitionMainMigration, RevisionTransitionIgnoredMigration)
			if table == "wisps" {
				mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM issues WHERE row_lock = 0 LIMIT 1")).
					WillReturnRows(sqlmock.NewRows([]string{"1"}))
			}
			mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM " + table + " WHERE row_lock = 0 LIMIT 1")).WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
			readiness, err := ProbeRevisionTransitionReadiness(context.Background(), db)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if readiness.Ready() {
				t.Fatalf("zero aggregate token in %s reported ready", table)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("receipt table unavailable means unknown", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectReadinessVersions(mock, RevisionTransitionMainMigration, RevisionTransitionIgnoredMigration)
		expectNoLegacyTokens(mock)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id FROM issue_transition_receipts LIMIT 0")).WillReturnError(errMissingReadinessTable)
		readiness, err := ProbeRevisionTransitionReadiness(context.Background(), db)
		if err == nil {
			t.Fatal("missing receipt table returned known readiness")
		}
		if readiness.Ready() {
			t.Fatal("missing receipt table reported ready")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("all prerequisites ready", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectReadinessVersions(mock, RevisionTransitionMainMigration, RevisionTransitionIgnoredMigration)
		expectNoLegacyTokens(mock)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id FROM issue_transition_receipts LIMIT 0")).WillReturnRows(sqlmock.NewRows([]string{"receipt_id"}))
		readiness, err := ProbeRevisionTransitionReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !readiness.Ready() {
			t.Fatal("complete migration state was not ready")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func expectReadinessVersions(mock sqlmock.Sqlmock, mainVersion, ignoredVersion int) {
	expectVersionCursor(mock, "schema_migrations", mainVersion)
	expectVersionCursor(mock, "ignored_schema_migrations", ignoredVersion)
}

func expectVersionCursor(mock sqlmock.Sqlmock, table string, version int) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?")).
		WithArgs(table).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM " + table)).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(version))
	if table == "ignored_schema_migrations" {
		for _, sentinel := range []string{"wisps", "wisp_dependencies"} {
			mock.ExpectQuery(`SELECT COUNT\(\*\) FROM INFORMATION_SCHEMA\.TABLES\s+WHERE TABLE_SCHEMA = DATABASE\(\) AND TABLE_NAME = \?`).
				WithArgs(sentinel).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		}
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM INFORMATION_SCHEMA\.COLUMNS\s+WHERE TABLE_SCHEMA = DATABASE\(\) AND TABLE_NAME = \? AND COLUMN_NAME = \?`).
			WithArgs("leases", "granted_node").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	}
}

func expectNoLegacyTokens(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM issues WHERE row_lock = 0 LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM wisps WHERE row_lock = 0 LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))
}
