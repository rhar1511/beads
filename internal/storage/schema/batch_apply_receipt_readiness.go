package schema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const BatchApplyReceiptMainMigration = 72

// BatchApplyReceiptReadiness is a startup proof that the durable result
// receipt table is ready. Its zero value is deliberately not ready.
type BatchApplyReceiptReadiness struct {
	mainVersion int
	verified    bool
}

// Ready reports whether ProbeBatchApplyReceiptReadiness verified migration
// 0072 and the receipt table.
func (r BatchApplyReceiptReadiness) Ready() bool {
	return r.verified && r.mainVersion >= BatchApplyReceiptMainMigration
}

// ProbeBatchApplyReceiptReadiness verifies the receipt schema using reads only.
// An error means readiness is unknown and must be treated as false.
func ProbeBatchApplyReceiptReadiness(ctx context.Context, db DBConn) (BatchApplyReceiptReadiness, error) {
	if db == nil {
		return BatchApplyReceiptReadiness{}, errors.New("schema: nil connection probing batch apply receipt readiness")
	}
	mainVersion, err := CurrentVersion(ctx, db)
	if err != nil {
		return BatchApplyReceiptReadiness{}, fmt.Errorf("read main schema version: %w", err)
	}
	if mainVersion < BatchApplyReceiptMainMigration {
		return BatchApplyReceiptReadiness{mainVersion: mainVersion}, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT receipt_id, request_digest, result_version, result_blob
		FROM issue_batch_apply_receipts LIMIT 0`)
	if err != nil {
		return BatchApplyReceiptReadiness{}, fmt.Errorf("verify issue batch apply receipt table: %w", err)
	}
	if err := rows.Close(); err != nil {
		return BatchApplyReceiptReadiness{}, fmt.Errorf("close issue batch apply receipt probe: %w", err)
	}
	primaryKeyReady, err := protectedMutationPrimaryKeyReady(ctx, db, "issue_batch_apply_receipts", []string{"receipt_id"})
	if err != nil {
		return BatchApplyReceiptReadiness{}, fmt.Errorf("verify issue batch apply receipt primary key: %w", err)
	}
	if !primaryKeyReady {
		return BatchApplyReceiptReadiness{mainVersion: mainVersion}, nil
	}
	columnsReady, err := batchApplyReceiptColumnsReady(ctx, db)
	if err != nil {
		return BatchApplyReceiptReadiness{}, fmt.Errorf("verify issue batch apply receipt columns: %w", err)
	}
	if !columnsReady {
		return BatchApplyReceiptReadiness{mainVersion: mainVersion}, nil
	}
	return BatchApplyReceiptReadiness{mainVersion: mainVersion, verified: true}, nil
}

func batchApplyReceiptColumnsReady(ctx context.Context, db DBConn) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT column_name, data_type, character_maximum_length, is_nullable
		FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'issue_batch_apply_receipts'
		AND column_name IN ('receipt_id', 'request_digest', 'result_version', 'result_blob')`)
	if err != nil {
		return false, err
	}
	type shape struct {
		dataType string
		max      sql.NullInt64
		nullable string
	}
	columns := make(map[string]shape, 4)
	for rows.Next() {
		var name string
		var got shape
		if err := rows.Scan(&name, &got.dataType, &got.max, &got.nullable); err != nil {
			_ = rows.Close()
			return false, err
		}
		columns[name] = got
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	want := map[string]struct {
		dataType string
		max      int64
	}{
		"receipt_id":     {dataType: "varbinary", max: 191},
		"request_digest": {dataType: "char", max: 64},
		"result_version": {dataType: "smallint"},
		"result_blob":    {dataType: "longblob"},
	}
	if len(columns) != len(want) {
		return false, nil
	}
	for name, expected := range want {
		got, ok := columns[name]
		if !ok || !strings.EqualFold(got.dataType, expected.dataType) || !strings.EqualFold(got.nullable, "NO") {
			return false, nil
		}
		if expected.max != 0 && (!got.max.Valid || got.max.Int64 != expected.max) {
			return false, nil
		}
	}
	return true, nil
}
