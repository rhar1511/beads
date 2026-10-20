//go:build cgo

package embeddeddolt_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/rhar1511/beads/internal/storage/schema"
)

func TestEmbeddedMigration0073ReplayPreservesReceipts(t *testing.T) {
	requireEmbedded(t)
	for _, shape := range []string{"both-present", "request-only", "receipt-only", "neither-present"} {
		t.Run(shape, func(t *testing.T) {
			ctx := t.Context()
			dataDir := seedMainSchemaAt(t, ctx, schema.LatestVersion())
			conn, closeConn := openPinnedConn(t, ctx, dataDir)
			defer closeConn()
			if shape == "receipt-only" || shape == "neither-present" {
				execFrozenGuard(t, ctx, conn, "ALTER TABLE issue_transition_receipts DROP COLUMN request_json")
			}
			if shape == "request-only" || shape == "neither-present" {
				execFrozenGuard(t, ctx, conn, "ALTER TABLE issue_transition_receipts DROP COLUMN receipt_json")
			}
			execFrozenGuard(t, ctx, conn, `INSERT INTO issue_transition_receipts
(receipt_id, issue_id, scope, kind, actor, metadata_key, from_revision, to_revision, expected_present, value_present, payload_json)
VALUES ('retained-receipt', 'retained-issue', 'private', 'metadata', 'test', 'key', 1, 2, 0, 0, '{"payload":"unchanged"}')`)
			if shape == "both-present" || shape == "request-only" {
				execFrozenGuard(t, ctx, conn, `UPDATE issue_transition_receipts SET request_json = '{"request":"unchanged"}'`)
			}
			if shape == "both-present" || shape == "receipt-only" {
				execFrozenGuard(t, ctx, conn, `UPDATE issue_transition_receipts SET receipt_json = '{"receipt":"unchanged"}'`)
			}
			execFrozenGuard(t, ctx, conn, "DELETE FROM schema_migrations WHERE version = 73")
			if err := schema.DrainCall(ctx, conn, "CALL DOLT_COMMIT('-Am', 'private 73 replay fixture')"); err != nil {
				t.Fatal(err)
			}
			if applied, err := schema.MigrateUp(ctx, conn); err != nil || applied != 1 {
				t.Fatalf("MigrateUp = (%d, %v), want one successful replay", applied, err)
			}
			if applied, err := schema.MigrateUp(ctx, conn); err != nil || applied != 0 {
				t.Fatalf("second MigrateUp = (%d, %v), want no-op", applied, err)
			}
			var payload, hash string
			var request, receipt sql.NullString
			if err := conn.QueryRowContext(ctx, `SELECT payload_json, request_json, receipt_json
FROM issue_transition_receipts WHERE receipt_id = 'retained-receipt'`).Scan(&payload, &request, &receipt); err != nil {
				t.Fatal(err)
			}
			if payload != `{"payload":"unchanged"}` {
				t.Fatalf("payload changed: %q", payload)
			}
			wantRequest := shape == "both-present" || shape == "request-only"
			wantReceipt := shape == "both-present" || shape == "receipt-only"
			if request.Valid != wantRequest || (request.Valid && request.String != `{"request":"unchanged"}`) || receipt.Valid != wantReceipt || (receipt.Valid && receipt.String != `{"receipt":"unchanged"}`) {
				t.Fatalf("receipt values changed: request=%v receipt=%v", request, receipt)
			}
			if err := conn.QueryRowContext(ctx, "SELECT content_hash FROM schema_migrations WHERE version = 73").Scan(&hash); err != nil {
				t.Fatal(err)
			}
			if hash != "00e860aa338b36f50a1a7b5fdc8da850471ed8fc91029dd4392d48e68421e210" {
				t.Fatalf("historical content hash changed: %s", hash)
			}
		})
	}
}

func TestEmbeddedMigration0073ReplayRefusesIncompatibleColumns(t *testing.T) {
	requireEmbedded(t)
	for _, column := range []string{"request_json", "receipt_json"} {
		for _, missingSibling := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sibling-missing-%t", column, missingSibling), func(t *testing.T) {
				ctx := t.Context()
				dataDir := seedMainSchemaAt(t, ctx, schema.LatestVersion())
				conn, closeConn := openPinnedConn(t, ctx, dataDir)
				defer closeConn()

				sibling := "request_json"
				if column == sibling {
					sibling = "receipt_json"
				}
				if missingSibling {
					execFrozenGuard(t, ctx, conn, "ALTER TABLE issue_transition_receipts DROP COLUMN "+sibling)
				}
				execFrozenGuard(t, ctx, conn, `INSERT INTO issue_transition_receipts
(receipt_id, issue_id, scope, kind, actor, metadata_key, from_revision, to_revision, expected_present, value_present, payload_json)
VALUES ('refusal-receipt', 'retained-issue', 'private', 'metadata', 'test', 'key', 1, 2, 0, 0, '{"payload":"unchanged"}')`)
				execFrozenGuard(t, ctx, conn, fmt.Sprintf("ALTER TABLE issue_transition_receipts MODIFY COLUMN %s TEXT NULL", column))
				execFrozenGuard(t, ctx, conn, "DELETE FROM schema_migrations WHERE version = 73")
				if err := schema.DrainCall(ctx, conn, "CALL DOLT_COMMIT('-Am', 'private incompatible 73 column')"); err != nil {
					t.Fatal(err)
				}
				if _, err := schema.MigrateUp(ctx, conn); err == nil || !strings.Contains(err.Error(), "incompatible definition") {
					t.Fatalf("MigrateUp error = %v, want incompatible-definition refusal", err)
				}
				var currentType string
				if err := conn.QueryRowContext(ctx, "SELECT COLUMN_TYPE FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'issue_transition_receipts' AND COLUMN_NAME = ?", column).Scan(&currentType); err != nil {
					t.Fatal(err)
				}
				if currentType != "text" {
					t.Fatalf("refused column was rewritten: %s", currentType)
				}
				var siblingCount, cursorCount int
				if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'issue_transition_receipts' AND COLUMN_NAME = ?", sibling).Scan(&siblingCount); err != nil {
					t.Fatal(err)
				}
				wantSibling := 1
				if missingSibling {
					wantSibling = 0
				}
				if siblingCount != wantSibling {
					t.Fatalf("refusal changed sibling existence: got %d want %d", siblingCount, wantSibling)
				}
				if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = 73").Scan(&cursorCount); err != nil {
					t.Fatal(err)
				}
				if cursorCount != 0 {
					t.Fatal("refused replay recorded migration73")
				}
				var payload string
				if err := conn.QueryRowContext(ctx, "SELECT payload_json FROM issue_transition_receipts WHERE receipt_id = 'refusal-receipt'").Scan(&payload); err != nil {
					t.Fatal(err)
				}
				if payload != `{"payload":"unchanged"}` {
					t.Fatalf("refusal changed receipt payload: %q", payload)
				}

			})
		}
	}
}

// The original SQL is frozen and non-idempotent. Keep that boundary visible:
// adapted execution is not evidence that the historical bytes are replay-safe.
func TestEmbeddedMigration0073FrozenSQLRequiresExecutionAdapter(t *testing.T) {
	requireEmbedded(t)
	ctx := t.Context()
	dataDir := seedMainSchemaAt(t, ctx, schema.LatestVersion())
	conn, closeConn := openPinnedConn(t, ctx, dataDir)
	defer closeConn()
	original, err := schema.MigrationSQL("0073_extend_issue_transition_receipts.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, original); err == nil || !strings.Contains(err.Error(), `Column "request_json" already exists`) {
		t.Fatalf("frozen73 direct replay = %v; expected duplicate-column boundary", err)
	}
}
