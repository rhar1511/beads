//go:build cgo

package embeddeddolt_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage/commitrows"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/storage/versioncontrolops"
	"github.com/steveyegge/beads/internal/types"
	publicops "github.com/steveyegge/beads/issueops"
)

func TestEmbeddedCreatePreservesPreexistingRowAndStageWork(t *testing.T) {
	for _, mode := range []string{"same source row dirty", "unrelated staged row"} {
		t.Run(mode, func(t *testing.T) {
			skipUnlessEmbeddedDolt(t)
			te := newTestEnv(t, "create_scope_guard")
			ctx := t.Context()
			for _, id := range []string{"scope-owner", "scope-unrelated"} {
				if err := te.store.CreateIssue(ctx, &types.Issue{ID: id, Title: "committed", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, "seed"); err != nil {
					t.Fatal(err)
				}
			}
			if err := te.store.Commit(ctx, "seed create scope"); err != nil {
				t.Fatal(err)
			}
			id := "scope-owner"
			if mode == "unrelated staged row" {
				id = "scope-unrelated"
			}
			te.exec(t, ctx, "UPDATE issues SET title = ? WHERE id = ?", "existing working", id)
			if mode == "unrelated staged row" {
				te.exec(t, ctx, "UPDATE dolt_workspace_issues SET staged = TRUE WHERE staged = FALSE AND to_id = ?", id)
			}
			operations, err := embeddeddolt.NewIssueOperations(te.store)
			if err != nil {
				t.Fatal(err)
			}
			_, err = operations.Create(ctx, publicops.CreateRequest{Actor: "writer", ForceIDPrefix: true, Issue: &types.Issue{ID: "scope-created", Title: "new", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, Dependencies: []publicops.CreateDependency{{TargetID: "scope-owner", Type: types.DepRelatesTo, Reverse: true}}})
			if !errors.Is(err, commitrows.ErrConflict) {
				t.Fatalf("create error = %v, want scope conflict", err)
			}
			var count int
			te.queryScalar(t, ctx, "SELECT COUNT(*) FROM issues WHERE id = ?", []any{"scope-created"}, &count)
			if count != 0 {
				t.Fatal("refused create committed data")
			}
			var title string
			te.queryScalar(t, ctx, "SELECT title FROM issues WHERE id = ?", []any{id}, &title)
			if title != "existing working" {
				t.Fatalf("prior WORKING changed: %q", title)
			}
			te.queryScalar(t, ctx, "SELECT title FROM issues AS OF 'HEAD' WHERE id = ?", []any{id}, &title)
			if title != "committed" {
				t.Fatalf("prior dirt reached HEAD: %q", title)
			}
			te.queryScalar(t, ctx, "SELECT COUNT(*) FROM dolt_workspace_issues WHERE staged = TRUE", nil, &count)
			want := 0
			if mode == "unrelated staged row" {
				want = 1
			}
			if count != want {
				t.Fatalf("staged rows = %d, want preserved %d", count, want)
			}
		})
	}
}

// This real-engine seam admits a late writer immediately before the final
// scope check; lower-layer row staging alone cannot prove this boundary.
func TestCreateStageFenceRetainsLateStagedInterference(t *testing.T) {
	for _, mode := range []string{"different issue staged", "owned staged image changed"} {
		t.Run(mode, func(t *testing.T) {
			skipUnlessEmbeddedDolt(t)
			te := newTestEnv(t, "create_final_stage")
			ctx := t.Context()
			for _, id := range []string{"fence-owned", "fence-other"} {
				if err := te.store.CreateIssue(ctx, &types.Issue{ID: id, Title: "committed", Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}, "seed"); err != nil {
					t.Fatal(err)
				}
			}
			if err := te.store.Commit(ctx, "seed final stage guard"); err != nil {
				t.Fatal(err)
			}
			db, cleanup, err := embeddeddolt.OpenSQL(ctx, te.dataDir, te.database, "main")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, "UPDATE issues SET title = ? WHERE id = ?", "owned working", "fence-owned"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE issues SET title = ? WHERE id = ?", "other working", "fence-other"); err != nil {
				t.Fatal(err)
			}
			images, err := commitrows.CaptureIssueRows(ctx, tx, []string{"fence-owned"})
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			intercepted := &stageFenceInterference{DB: db, beforeVerify: func() error {
				id := "fence-other"
				if mode == "owned staged image changed" {
					id = "fence-owned"
					if _, err := db.ExecContext(ctx, "UPDATE issues SET title = ? WHERE id = ?", "later owned writer", id); err != nil {
						return err
					}
				}
				_, err := db.ExecContext(ctx, "UPDATE dolt_workspace_issues SET staged = TRUE WHERE staged = FALSE AND to_id = ?", id)
				return err
			}}
			err = versioncontrolops.StageCreateAndCommit(ctx, intercepted, map[string]bool{"issues": true}, images, "must not commit late interference", "Test <test@example.com>")
			if !errors.Is(err, commitrows.ErrConflict) {
				t.Fatalf("stage fence error = %v, want conflict", err)
			}
			if intercepted.beforeVerify != nil {
				t.Fatal("late-stage boundary was not reached")
			}
			for _, id := range []string{"fence-owned", "fence-other"} {
				var title string
				if err := db.QueryRowContext(ctx, "SELECT title FROM issues AS OF 'HEAD' WHERE id = ?", id).Scan(&title); err != nil {
					t.Fatal(err)
				}
				if title != "committed" {
					t.Fatalf("late stage was committed for %s: %q", id, title)
				}
				if err := db.QueryRowContext(ctx, "SELECT title FROM issues WHERE id = ?", id).Scan(&title); err != nil {
					t.Fatal(err)
				}
				want := "other working"
				if id == "fence-owned" {
					want = "owned working"
					if mode == "owned staged image changed" {
						want = "later owned writer"
					}
				}
				if title != want {
					t.Fatalf("late work lost for %s: %q want %q", id, title, want)
				}
			}
			var staged int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_workspace_issues WHERE staged = TRUE").Scan(&staged); err != nil {
				t.Fatal(err)
			}
			want := 2
			if mode == "owned staged image changed" {
				want = 1
			}
			if staged != want {
				t.Fatalf("staged progress lost: %d want %d", staged, want)
			}
		})
	}
}

type stageFenceInterference struct {
	*sql.DB
	beforeVerify func() error
}

func (c *stageFenceInterference) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if query == "SELECT table_name FROM dolt_status WHERE staged = TRUE" && c.beforeVerify != nil {
		inject := c.beforeVerify
		c.beforeVerify = nil
		if err := inject(); err != nil {
			return nil, err
		}
	}
	return c.DB.QueryContext(ctx, query, args...)
}
