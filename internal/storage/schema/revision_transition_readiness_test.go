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
		{name: "main receipt migration absent", mainVersion: RevisionTransitionMainMigration - 1, ignoredVer: RevisionTransitionIgnoredMigration},
		{name: "ignored zero-token backfill absent", mainVersion: RevisionTransitionMainMigration, ignoredVer: RevisionTransitionIgnoredMigration - 1},
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
	if readiness.PatchReady() {
		t.Fatal("zero-value readiness proof is patch-ready")
	}
}

func TestPatchReadinessRequiresItsAdditiveMigrationAndColumns(t *testing.T) {
	t.Run("revision-transition schema alone keeps patch hidden", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectReadinessVersions(mock, RevisionTransitionPatchMigration-1, RevisionTransitionIgnoredMigration)
		expectNoLegacyTokens(mock)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id FROM issue_transition_receipts LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"receipt_id"}))
		readiness, err := ProbeRevisionTransitionReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !readiness.Ready() || readiness.PatchReady() {
			t.Fatalf("readiness = %+v, want legacy API ready and patch unavailable", readiness)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("column probe fails closed", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectReadinessVersions(mock, RevisionTransitionPatchMigration, RevisionTransitionIgnoredMigration)
		expectNoLegacyTokens(mock)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id FROM issue_transition_receipts LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"receipt_id"}))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT request_json, receipt_json FROM issue_transition_receipts LIMIT 0")).
			WillReturnError(errMissingReadinessTable)
		readiness, err := ProbeRevisionTransitionReadiness(context.Background(), db)
		if err == nil || readiness.PatchReady() {
			t.Fatalf("probe = %+v, %v; want unready with a column error", readiness, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("migration and columns verified", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectReadinessVersions(mock, RevisionTransitionPatchMigration, RevisionTransitionIgnoredMigration)
		expectNoLegacyTokens(mock)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id FROM issue_transition_receipts LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"receipt_id"}))
		mock.ExpectQuery(regexp.QuoteMeta("SELECT request_json, receipt_json FROM issue_transition_receipts LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"request_json", "receipt_json"}))
		readiness, err := ProbeRevisionTransitionReadiness(context.Background(), db)
		if err != nil || !readiness.PatchReady() {
			t.Fatalf("probe = %+v, %v; want patch ready", readiness, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
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
		expectIgnoredSentinelProbes(mock, true)
	}
}

func expectNoLegacyTokens(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM issues WHERE row_lock = 0 LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM wisps WHERE row_lock = 0 LIMIT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))
}
