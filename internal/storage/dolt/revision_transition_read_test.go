package dolt

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRevisionTransitionReceiptReadRollsBackReadOnlyTransaction(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT receipt_id, issue_id, scope, kind, actor, metadata_key,
		       from_revision, to_revision, expected_present, expected_json,
		       value_present, value_json, payload_json
		FROM issue_transition_receipts WHERE receipt_id = ?
	`)).WithArgs("receipt-1").WillReturnRows(sqlmock.NewRows([]string{
		"receipt_id", "issue_id", "scope", "kind", "actor", "metadata_key",
		"from_revision", "to_revision", "expected_present", "expected_json",
		"value_present", "value_json", "payload_json",
	}).AddRow("receipt-1", "bd-1", "scope", "kind", "actor", "gc.k",
		int64(-1), int64(1), 0, nil, 1, `"next"`, `{}`))
	mock.ExpectRollback()

	role := &revisionTransition{store: &DoltStore{db: db}}
	receipt, err := role.GetReceipt(context.Background(), "receipt-1")
	if err != nil {
		t.Fatalf("GetReceipt: %v", err)
	}
	if receipt == nil || receipt.ReceiptID != "receipt-1" {
		t.Fatalf("GetReceipt = %#v, want receipt-1", receipt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("receipt recovery did not remain read-only: %v", err)
	}
}
