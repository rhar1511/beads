package schema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	migration0073Name = "0073_extend_issue_transition_receipts.up.sql"
	migration0073Hash = "00e860aa338b36f50a1a7b5fdc8da850471ed8fc91029dd4392d48e68421e210"
)

// validateMigration0073Execution preserves the shipped file and the hash that
// runMigrations records. This repair is only for that exact main migration.
func validateMigration0073Execution(src migrationSource, mf migrationFile, original []byte) error {
	if src.cursorTable != mainSource.cursorTable || src.dir != mainSource.dir || mf.version != 73 {
		return fmt.Errorf("migration 0073 execution requested for non-target source/version")
	}
	if mf.name != migration0073Name {
		return fmt.Errorf("main migration 0073 has unexpected filename %q", mf.name)
	}
	sum := sha256.Sum256(original)
	if got := hex.EncodeToString(sum[:]); got != migration0073Hash {
		return fmt.Errorf("migration %s has unexpected content hash %s (want %s)", mf.name, got, migration0073Hash)
	}
	return nil
}

// execMigration0073 converges only the two nullable LONGTEXT receipt columns.
// An interrupted ALTER or lost cursor row can leave either or both present.
// Inspect both before any DDL: incompatible existing columns are errors, not
// permission to drop, rewrite or coerce receipt data. The caller's ordinary
// migration lock, consent gate and step commit remain responsible for admission.
func execMigration0073(ctx context.Context, db DBConn) error {
	present, err := migration0073Columns(ctx, db)
	if err != nil {
		return err
	}
	var additions []string
	for _, name := range []string{"request_json", "receipt_json"} {
		if !present[name] {
			additions = append(additions, "ADD COLUMN "+name+" LONGTEXT NULL")
		}
	}
	if len(additions) == 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE issue_transition_receipts "+strings.Join(additions, ", ")); err != nil {
		return fmt.Errorf("add missing migration 0073 receipt columns: %w", err)
	}
	present, err = migration0073Columns(ctx, db)
	if err != nil {
		return err
	}
	if !present["request_json"] || !present["receipt_json"] {
		return fmt.Errorf("migration 0073 receipt columns missing after ALTER")
	}
	return nil
}

func migration0073Columns(ctx context.Context, db DBConn) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA
FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'issue_transition_receipts'
  AND COLUMN_NAME IN ('request_json', 'receipt_json')`)
	if err != nil {
		return nil, fmt.Errorf("inspect migration 0073 receipt columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	present := make(map[string]bool, 2)
	for rows.Next() {
		var name, columnType, nullable, extra string
		var defaultValue sql.NullString
		if err := rows.Scan(&name, &columnType, &nullable, &defaultValue, &extra); err != nil {
			return nil, fmt.Errorf("scan migration 0073 receipt column: %w", err)
		}
		if (name != "request_json" && name != "receipt_json") || present[name] {
			return nil, fmt.Errorf("unexpected migration 0073 receipt column %q", name)
		}
		if !strings.EqualFold(columnType, "longtext") || nullable != "YES" || defaultValue.Valid || extra != "" {
			return nil, fmt.Errorf("migration 0073 receipt column %s has incompatible definition", name)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read migration 0073 receipt columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close migration 0073 receipt columns: %w", err)
	}
	return present, nil
}
