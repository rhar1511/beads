package schema

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestBatchApplyReceiptReadinessRequiresMigrationTableAndPrimaryKey(t *testing.T) {
	t.Run("migration absent", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectVersionCursor(mock, "schema_migrations", BatchApplyReceiptMainMigration-1)
		readiness, err := ProbeBatchApplyReceiptReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if readiness.Ready() {
			t.Fatal("pre-migration database reported receipt readiness")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("receipt table unavailable", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectVersionCursor(mock, "schema_migrations", BatchApplyReceiptMainMigration)
		missing := errMissingReadinessTable
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id, request_digest, result_version, result_blob\n\t\tFROM issue_batch_apply_receipts LIMIT 0")).
			WillReturnError(missing)
		readiness, err := ProbeBatchApplyReceiptReadiness(context.Background(), db)
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

	t.Run("primary key is not receipt id", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectVersionCursor(mock, "schema_migrations", BatchApplyReceiptMainMigration)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id, request_digest, result_version, result_blob\n\t\tFROM issue_batch_apply_receipts LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"receipt_id", "request_digest", "result_version", "result_blob"}))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`)).WithArgs("issue_batch_apply_receipts").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("other_column"))
		readiness, err := ProbeBatchApplyReceiptReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if readiness.Ready() {
			t.Fatal("wrong receipt primary key reported ready")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("primary key has an extra component", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectVersionCursor(mock, "schema_migrations", BatchApplyReceiptMainMigration)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id, request_digest, result_version, result_blob\n\t\tFROM issue_batch_apply_receipts LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"receipt_id", "request_digest", "result_version", "result_blob"}))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`)).WithArgs("issue_batch_apply_receipts").
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("receipt_id").AddRow("request_digest"))
		readiness, err := ProbeBatchApplyReceiptReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if readiness.Ready() {
			t.Fatal("composite receipt primary key reported ready")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("receipt identity column has the wrong shape", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expectVersionCursor(mock, "schema_migrations", BatchApplyReceiptMainMigration)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id, request_digest, result_version, result_blob\n\t\tFROM issue_batch_apply_receipts LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"receipt_id", "request_digest", "result_version", "result_blob"}))
		expectBatchApplyReceiptPrimaryKey(mock, "receipt_id")
		expectBatchApplyReceiptColumns(mock, "varchar", int64(191))
		readiness, err := ProbeBatchApplyReceiptReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if readiness.Ready() {
			t.Fatal("wrong receipt identity column shape reported ready")
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
		expectVersionCursor(mock, "schema_migrations", BatchApplyReceiptMainMigration)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT receipt_id, request_digest, result_version, result_blob\n\t\tFROM issue_batch_apply_receipts LIMIT 0")).
			WillReturnRows(sqlmock.NewRows([]string{"receipt_id", "request_digest", "result_version", "result_blob"}))
		expectBatchApplyReceiptPrimaryKey(mock, "receipt_id")
		expectBatchApplyReceiptColumns(mock, "varbinary", int64(191))
		readiness, err := ProbeBatchApplyReceiptReadiness(context.Background(), db)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !readiness.Ready() {
			t.Fatal("ready receipt table was not reported ready")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func expectBatchApplyReceiptPrimaryKey(mock sqlmock.Sqlmock, columns ...string) {
	rows := sqlmock.NewRows([]string{"column_name"})
	for _, column := range columns {
		rows.AddRow(column)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = 'PRIMARY'
		ORDER BY seq_in_index`)).WithArgs("issue_batch_apply_receipts").WillReturnRows(rows)
}

func expectBatchApplyReceiptColumns(mock sqlmock.Sqlmock, receiptType string, receiptLength any) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name, data_type, character_maximum_length, is_nullable
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'issue_batch_apply_receipts'
		AND column_name IN ('receipt_id', 'request_digest', 'result_version', 'result_blob')`)).
		WillReturnRows(sqlmock.NewRows([]string{"column_name", "data_type", "character_maximum_length", "is_nullable"}).
			AddRow("receipt_id", receiptType, receiptLength, "NO").
			AddRow("request_digest", "char", int64(64), "NO").
			AddRow("result_version", "smallint", nil, "NO").
			AddRow("result_blob", "longblob", nil, "NO"))
}

func TestZeroBatchApplyReceiptReadinessIsUnknown(t *testing.T) {
	var readiness BatchApplyReceiptReadiness
	if readiness.Ready() {
		t.Fatal("zero-value readiness proof is ready")
	}
}
