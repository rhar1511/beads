//go:build cgo

package embeddeddolt_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/storage/schema"
)

// Exercise the actual published66 -> published67 -> admission68..73 jump.
// The closed private-store copies also demonstrate restore of both durable
// and clone-local data; they do not certify an online production backup.
func TestEmbeddedAdmissionUpgradeFrom66AndRestore(t *testing.T) {
	requireEmbedded(t)
	ctx := t.Context()
	dataDir := seedMainSchemaAt(t, ctx, 66)
	conn, closeConn := openDepConn(t, ctx, dataDir)
	seedIssue(t, ctx, conn, "upgrade-source")
	seedIssue(t, ctx, conn, "upgrade-target")
	seedWisp(t, ctx, conn, "upgrade-wisp")
	seedDependency(t, ctx, conn, "legacy-edge", "upgrade-source", strptr("upgrade-target"), nil, nil)
	mustExecConn(t, ctx, conn, "UPDATE issues SET row_lock = 0, metadata = JSON_OBJECT('preserved', 'yes') WHERE id = 'upgrade-source'")
	mustExecConn(t, ctx, conn, "UPDATE wisps SET row_lock = 0 WHERE id = 'upgrade-wisp'")
	commitSeed(t, ctx, conn, "test: admission upgrade66 seed")
	beforeHead := scalarString(t, ctx, conn, "SELECT commit_hash FROM dolt_log LIMIT 1")
	closeConn()
	backup := filepath.Join(t.TempDir(), "backup")
	if err := os.CopyFS(backup, os.DirFS(dataDir)); err != nil {
		t.Fatal(err)
	}
	conn, closeConn = openDepConn(t, ctx, dataDir)
	if _, err := schema.MigrateUp(ctx, conn); err != nil {
		t.Fatalf("upgrade from66: %v", err)
	}
	if got, err := schema.CurrentVersion(ctx, conn); err != nil || got != 73 {
		t.Fatalf("main version=%d err=%v", got, err)
	}
	if got := scalarInt(t, ctx, conn, "SELECT MAX(version) FROM ignored_schema_migrations"); got != 28 {
		t.Fatalf("ignored version=%d", got)
	}
	if got := scalarString(t, ctx, conn, "SELECT JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.preserved')) FROM issues WHERE id = 'upgrade-source'"); got != "yes" {
		t.Fatalf("metadata=%q", got)
	}
	if got := scalarInt(t, ctx, conn, "SELECT row_lock FROM issues WHERE id = 'upgrade-source'"); got == 0 {
		t.Fatal("durable revision not backfilled")
	}
	if got := scalarInt(t, ctx, conn, "SELECT row_lock FROM wisps WHERE id = 'upgrade-wisp'"); got == 0 {
		t.Fatal("wisp revision not backfilled")
	}
	if got := scalarInt(t, ctx, conn, "SELECT COUNT(*) FROM dependencies WHERE issue_id = 'upgrade-source' AND depends_on_issue_id = 'upgrade-target'"); got != 1 {
		t.Fatalf("edges=%d", got)
	}
	proof, err := schema.ProbeRevisionTransitionReadiness(ctx, conn)
	if err != nil || !proof.PatchReady() {
		t.Fatalf("patch readiness=%+v err=%v", proof, err)
	}
	if _, err := schema.MigrateUp(ctx, conn); err != nil {
		t.Fatalf("idempotent upgrade: %v", err)
	}
	closeConn()
	restored := filepath.Join(t.TempDir(), "restored")
	if err := os.CopyFS(restored, os.DirFS(backup)); err != nil {
		t.Fatal(err)
	}
	conn, _ = openDepConn(t, ctx, restored)
	if got, err := schema.CurrentVersion(ctx, conn); err != nil || got != 66 {
		t.Fatalf("restored version=%d err=%v", got, err)
	}
	if got := scalarString(t, ctx, conn, "SELECT commit_hash FROM dolt_log LIMIT 1"); got != beforeHead {
		t.Fatalf("restored head=%q want=%q", got, beforeHead)
	}
	if got := scalarInt(t, ctx, conn, "SELECT row_lock FROM issues WHERE id = 'upgrade-source'"); got != 0 {
		t.Fatalf("restored durable revision=%d", got)
	}
	if got := scalarInt(t, ctx, conn, "SELECT row_lock FROM wisps WHERE id = 'upgrade-wisp'"); got != 0 {
		t.Fatalf("restored clone-local revision=%d", got)
	}
	if got := scalarString(t, ctx, conn, "SELECT JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.preserved')) FROM issues WHERE id = 'upgrade-source'"); got != "yes" {
		t.Fatalf("restored metadata=%q", got)
	}
	if got := scalarInt(t, ctx, conn, "SELECT COUNT(*) FROM dependencies WHERE id = 'legacy-edge'"); got != 1 {
		t.Fatalf("restored original edge=%d", got)
	}
}

// Installing a revision token is bookkeeping, not a user edit. Execute the
// exact backfill SQL against real ON UPDATE columns on both storage planes.
func TestEmbeddedRevisionBackfillsPreserveEditTimes(t *testing.T) {
	requireEmbedded(t)
	ctx := t.Context()
	dataDir := seedMigratedStore(t, ctx)
	conn, _ := openDepConn(t, ctx, dataDir)
	seedIssue(t, ctx, conn, "edit-source")
	seedWisp(t, ctx, conn, "edit-wisp")
	for _, tc := range []struct {
		table, id, migration string
		ignored              bool
	}{
		{"issues", "edit-source", "0068_backfill_aggregate_row_lock.up.sql", false},
		{"wisps", "edit-wisp", "0028_backfill_wisp_aggregate_row_lock.up.sql", true},
	} {
		t.Run(tc.table, func(t *testing.T) {
			mustExecConn(t, ctx, conn, fmt.Sprintf("UPDATE %s SET row_lock = 0, created_at = '2020-01-02 03:04:05', updated_at = '2020-01-03 04:05:06' WHERE id = ?", tc.table), tc.id)
			migrationSQL := schema.MigrationSQL
			if tc.ignored {
				migrationSQL = schema.IgnoredMigrationSQL
			}
			sql, err := migrationSQL(tc.migration)
			if err != nil {
				t.Fatal(err)
			}
			execFrozenGuard(t, ctx, conn, sql)
			if got := scalarInt(t, ctx, conn, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE row_lock = 1 AND created_at = '2020-01-02 03:04:05' AND updated_at = '2020-01-03 04:05:06'", tc.table)); got != 1 {
				t.Fatalf("%s backfill did not preserve creation/edit timestamps while installing the revision", tc.table)
			}
		})
	}
}
